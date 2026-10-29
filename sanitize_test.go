package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"howett.net/plist"
)

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

// What `unfairplay decrypt --strip-account-data` writes passes the check;
// an archive that still has licenses or the Apple ID does not.
func TestVerifySanitized(t *testing.T) {
	dir := t.TempDir()
	dirty, clean := filepath.Join(dir, "dirty.ipa"), filepath.Join(dir, "clean.ipa")
	if err := writeIPA(dirty, target(1, "1")); err != nil {
		t.Fatal(err)
	}
	if err := verifySanitized(dirty); classify(err).Code != "sanitize_failed" {
		t.Fatalf("unsanitized IPA: got %v", err)
	}
	res, err := fakeStripAccountData(dirty, clean)
	if err != nil || res.RemovedLicenseFiles != 4 || res.RemovedMetadataKeys != 3 {
		t.Fatalf("fake strip: %+v %v", res, err)
	}
	if err := verifySanitized(clean); err != nil {
		t.Fatalf("sanitized IPA failed: %v", err)
	}
	zr, err := zip.OpenReader(clean)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "iTunesMetadata.plist" {
			continue
		}
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		var meta map[string]any
		if _, err := plist.Unmarshal(data, &meta); err != nil {
			t.Fatal(err)
		}
		if meta["itemName"] != "Example" {
			t.Fatalf("app metadata lost: %v", meta)
		}
	}
}

// Without the flag the service must not run at all: an older unfairplay would
// put the account's licenses and Apple ID into every cached IPA.
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
