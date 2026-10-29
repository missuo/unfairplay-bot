package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"

	"howett.net/plist"
)

type sanitizeResult struct {
	RemovedLicenseFiles int
	RemovedMetadataKeys int
}

// fakeStripAccountData does what `unfairplay decrypt --strip-account-data`
// does to the output archive, for the fake pipeline.
func fakeStripAccountData(src, dst string) (sanitizeResult, error) {
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
	return res, nil
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
