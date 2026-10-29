package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"strings"

	"howett.net/plist"
)

// Account data the decrypted IPA must not carry. SC_Info holds the
// personalized licenses (SINF/SUPF), which identify the Apple ID and are not
// needed once the binaries are decrypted. iTunesMetadata.plist gets the Apple
// ID email from ipatool and purchaser details from the App Store.
var (
	metadataAccountKeys = []string{
		"apple-id", "appleId", "AppleID", "userName", "UserName",
		"purchaserID", "DSPersonID", "dsPersonId", "AltDSID", "altDsid", "FamilyID", "familyId",
	}
	downloadInfoKey = "com.apple.iTunesStore.downloadInfo"
)

// isLicensePath reports whether an archive entry is inside an SC_Info
// directory (of the app, an extension, a watch app, or anything else).
func isLicensePath(name string) bool {
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if strings.EqualFold(part, "SC_Info") {
			return true
		}
	}
	return false
}

type sanitizeResult struct {
	RemovedLicenseFiles int
	RemovedMetadataKeys int
}

// sanitizeIPA copies src to dst without SC_Info entries and with the account
// fields removed from iTunesMetadata.plist. Other entries are copied raw,
// without recompressing.
func sanitizeIPA(src, dst string) (sanitizeResult, error) {
	var res sanitizeResult
	zr, err := zip.OpenReader(src)
	if err != nil {
		return res, fmt.Errorf("open decrypted ipa: %w", err)
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return res, err
	}
	defer out.Close()
	zw := zip.NewWriter(out)

	for _, f := range zr.File {
		switch {
		case isLicensePath(f.Name):
			if !f.FileInfo().IsDir() {
				res.RemovedLicenseFiles++
			}
		case f.Name == "iTunesMetadata.plist":
			data, removed, err := scrubMetadata(f)
			if err != nil {
				return res, err
			}
			res.RemovedMetadataKeys = removed
			header := f.FileHeader
			header.Method = zip.Deflate
			w, err := zw.CreateHeader(&header)
			if err != nil {
				return res, err
			}
			if _, err := w.Write(data); err != nil {
				return res, err
			}
		default:
			if err := zw.Copy(f); err != nil {
				return res, fmt.Errorf("copy %s: %w", f.Name, err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		return res, err
	}
	if err := out.Sync(); err != nil {
		return res, err
	}
	if err := out.Close(); err != nil {
		return res, err
	}
	return res, verifySanitized(dst)
}

func scrubMetadata(f *zip.File) ([]byte, int, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	rc.Close()
	if err != nil {
		return nil, 0, err
	}
	var meta map[string]any
	format, err := plist.Unmarshal(data, &meta)
	if err != nil {
		return nil, 0, fmt.Errorf("parse iTunesMetadata.plist: %w", err)
	}
	removed := 0
	for _, key := range metadataAccountKeys {
		if _, ok := meta[key]; ok {
			delete(meta, key)
			removed++
		}
	}
	if info, ok := meta[downloadInfoKey].(map[string]any); ok {
		if _, ok := info["accountInfo"]; ok {
			delete(info, "accountInfo")
			removed++
		}
	}
	data, err = plist.Marshal(meta, format)
	if err != nil {
		return nil, 0, fmt.Errorf("write iTunesMetadata.plist: %w", err)
	}
	return data, removed, nil
}

// verifySanitized fails closed if account data survived the repack.
func verifySanitized(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if isLicensePath(f.Name) {
			return &TaskError{Code: "sanitize_failed", Message: "license file left in output: " + f.Name}
		}
		if f.Name != "iTunesMetadata.plist" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			return err
		}
		var meta map[string]any
		if _, err := plist.Unmarshal(data, &meta); err != nil {
			return err
		}
		for _, key := range metadataAccountKeys {
			if _, ok := meta[key]; ok {
				return &TaskError{Code: "sanitize_failed", Message: "account field left in iTunesMetadata.plist: " + key}
			}
		}
		if info, ok := meta[downloadInfoKey].(map[string]any); ok && info["accountInfo"] != nil {
			return &TaskError{Code: "sanitize_failed", Message: "accountInfo left in iTunesMetadata.plist"}
		}
	}
	return nil
}
