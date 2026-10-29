package main

import (
	"archive/zip"
	"io"
	"strings"

	"howett.net/plist"
)

// Account data the decrypted IPA must not carry. SC_Info holds the
// personalized licenses (SINF/SUPF), which identify the Apple ID and are not
// needed once the binaries are decrypted. iTunesMetadata.plist gets the Apple
// ID email from ipatool and purchaser details from the App Store.
//
// `unfairplay decrypt --strip-account-data` removes both while packaging
// (DrWhax/unfairplay#20); the service only verifies the result.
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

// verifySanitized fails closed if account data survived decryption.
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
