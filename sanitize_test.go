package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestSanitizeIPARemovesLicensesAndAccountData(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "in.ipa"), filepath.Join(dir, "out.ipa")
	if err := writeIPA(src, target(1, "200")); err != nil {
		t.Fatal(err)
	}
	res, err := sanitizeIPA(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedLicenseFiles != 4 || res.RemovedMetadataKeys != 3 {
		t.Fatalf("result %+v, want 4 license files and 3 metadata fields", res)
	}

	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	var meta map[string]any
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.Contains(strings.ToLower(f.Name), "sc_info") {
			t.Errorf("license entry kept: %s", f.Name)
		}
		if f.Name == "iTunesMetadata.plist" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(data), "me@example.com") {
				t.Errorf("Apple ID left in metadata:\n%s", data)
			}
			if _, err := plist.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(names) != 2 || names[1] != "Payload/Example.app/Example" {
		t.Fatalf("entries %v", names)
	}
	// App metadata survives; so does non-account download info.
	if meta["itemName"] != "Example" || meta["bundleShortVersionString"] != "8.0.200" {
		t.Fatalf("metadata lost app fields: %v", meta)
	}
	info := meta["com.apple.iTunesStore.downloadInfo"].(map[string]any)
	if _, ok := info["accountInfo"]; ok || info["purchaseDate"] == nil {
		t.Fatalf("downloadInfo = %v", info)
	}
	// describeIPA still works on the sanitized file.
	if a, err := describeIPA(dst, target(1, "200")); err != nil || a.Version != "8.0.200" {
		t.Fatalf("describeIPA: %+v %v", a, err)
	}
}

func TestIsLicensePath(t *testing.T) {
	for name, want := range map[string]bool{
		"Payload/A.app/SC_Info/":                       true,
		"Payload/A.app/SC_Info/A.sinf":                 true,
		"Payload/A.app/PlugIns/B.appex/sc_info/B.sinf": true,
		"Payload/A.app/SC_Information.txt":             false,
		"Payload/A.app/Assets.car":                     false,
	} {
		if got := isLicensePath(name); got != want {
			t.Errorf("isLicensePath(%q) = %v", name, got)
		}
	}
}

// A sanitized IPA (as a recent unfairplay produces with
// --strip-account-data) passes the check, so the task skips the repack;
// an unsanitized one does not.
func TestVerifySanitizedDecidesRepack(t *testing.T) {
	dir := t.TempDir()
	dirty, clean := filepath.Join(dir, "dirty.ipa"), filepath.Join(dir, "clean.ipa")
	if err := writeIPA(dirty, target(1, "1")); err != nil {
		t.Fatal(err)
	}
	if verifySanitized(dirty) == nil {
		t.Fatal("unsanitized IPA passed")
	}
	if _, err := sanitizeIPA(dirty, clean); err != nil {
		t.Fatal(err)
	}
	if err := verifySanitized(clean); err != nil {
		t.Fatalf("sanitized IPA failed: %v", err)
	}
}

func TestSupportsStripAccountData(t *testing.T) {
	dir := t.TempDir()
	write := func(name, help string) string {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\ncat <<'X'\n" + help + "\nX\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if !supportsStripAccountData(write("new", "      --strip-account-data   Omit SC_Info licenses")) {
		t.Fatal("flag not detected")
	}
	if supportsStripAccountData(write("old", "      --force   Replace existing output")) {
		t.Fatal("flag detected in an old unfairplay")
	}
	if supportsStripAccountData(filepath.Join(dir, "missing")) {
		t.Fatal("flag detected for a missing binary")
	}
}
