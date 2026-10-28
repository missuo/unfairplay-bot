package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"howett.net/plist"
)

// Decrypter runs `unfairplay decrypt`. UnfairPlay's packages are internal, so
// the CLI is the supported interface.
type Decrypter struct {
	Bin   string
	Model string
}

func (d Decrypter) Decrypt(ctx context.Context, input, output string) error {
	report := filepath.Join(filepath.Dir(output), "unfairplay-report.json")
	cmd := exec.CommandContext(ctx, d.Bin, "decrypt", "--input", input, "--output", output, "--report", report, "--model", d.Model)
	var stderr tailBuffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return &TaskError{Code: "decrypt_failed", Message: msg}
	}
	return nil
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

type pipeline struct {
	*Store
	Decrypter
}

// describeIPA builds cache metadata from the decrypted IPA. The build key
// stays the one the task was resolved with; the metadata only adds display
// fields and is checked against it.
func describeIPA(path string, target Target) (Artifact, error) {
	a := Artifact{Key: target.Key, AppID: target.AppID, BundleID: target.BundleID, Name: target.Name, ExternalVersionID: target.ExternalVersionID}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return a, fmt.Errorf("open decrypted ipa: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "iTunesMetadata.plist" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return a, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
		rc.Close()
		if err != nil {
			return a, err
		}
		var meta map[string]any
		if _, err := plist.Unmarshal(data, &meta); err != nil {
			return a, fmt.Errorf("parse iTunesMetadata.plist: %w", err)
		}
		if ext := fmt.Sprint(meta["softwareVersionExternalIdentifier"]); meta["softwareVersionExternalIdentifier"] != nil && ext != target.ExternalVersionID {
			return a, &TaskError{Code: "version_mismatch", Message: fmt.Sprintf("downloaded build %s, expected %s", ext, target.ExternalVersionID)}
		}
		if v, ok := meta["bundleShortVersionString"].(string); ok {
			a.Version = v
		}
		if v, ok := meta["softwareVersionBundleId"].(string); ok && a.BundleID == "" {
			a.BundleID = v
		}
		if v, ok := meta["itemName"].(string); ok && a.Name == "" {
			a.Name = v
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return a, err
	}
	defer f.Close()
	h := sha256.New()
	if a.Size, err = io.Copy(h, f); err != nil {
		return a, err
	}
	a.SHA256 = hex.EncodeToString(h.Sum(nil))
	a.FileName = artifactFileName(a)
	return a, nil
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func artifactFileName(a Artifact) string {
	base := a.BundleID
	if base == "" {
		base = fmt.Sprint(a.AppID)
	}
	if a.Version != "" {
		base += "_" + a.Version
	}
	return strings.Trim(unsafeFileChars.ReplaceAllString(base, "_"), "._") + ".decrypted.ipa"
}
