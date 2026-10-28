package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/majd/ipatool/v2/pkg/appstore"
)

// AppInfo is what a user confirms before a task is created. The version
// fields come from the App Store download endpoint; the descriptive fields
// come from the public iTunes lookup API and may be missing.
type AppInfo struct {
	Target
	Developer   string    `json:"developer,omitempty"`
	ReleaseDate time.Time `json:"release_date,omitzero"`
	Size        int64     `json:"size,omitempty"`
	Price       float64   `json:"price"`
	PriceText   string    `json:"price_text,omitempty"`
	IconURL     string    `json:"icon_url,omitempty"`
	MinOS       string    `json:"min_os,omitempty"`
}

type itunesApp struct {
	TrackID        int64   `json:"trackId"`
	BundleID       string  `json:"bundleId"`
	TrackName      string  `json:"trackName"`
	SellerName     string  `json:"sellerName"`
	ArtistName     string  `json:"artistName"`
	FileSizeBytes  string  `json:"fileSizeBytes"`
	Price          float64 `json:"price"`
	FormattedPrice string  `json:"formattedPrice"`
	Artwork        string  `json:"artworkUrl512"`
	MinimumOS      string  `json:"minimumOsVersion"`
}

func (s *Store) itunesLookup(ctx context.Context, bundleID string, appID int64) (*itunesApp, error) {
	q := url.Values{"country": {s.country}, "entity": {"software"}, "limit": {"1"}}
	if bundleID != "" {
		q.Set("bundleId", bundleID)
	} else {
		q.Set("id", strconv.FormatInt(appID, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://itunes.apple.com/lookup?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("itunes lookup: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Results []itunesApp `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Results) == 0 {
		return nil, nil
	}
	return &body.Results[0], nil
}

// Resolve finds the app and the build the App Store would serve right now.
// The build comes from the same download endpoint ipatool downloads from, not
// from the iTunes lookup API, whose CDN can lag behind a release.
func (s *Store) Resolve(ctx context.Context, bundleID string, appID int64) (AppInfo, error) {
	info := AppInfo{Target: Target{AppID: appID, BundleID: bundleID}}
	// Descriptive data only; a failure here must not block the download.
	if it, err := s.itunesLookup(ctx, bundleID, appID); err == nil && it != nil {
		if appID != 0 && it.TrackID != appID {
			return AppInfo{}, &TaskError{Code: "bad_request", Message: fmt.Sprintf("bundle_id %s belongs to app_id %d, not %d", bundleID, it.TrackID, appID)}
		}
		info.AppID, info.BundleID, info.Name = it.TrackID, it.BundleID, it.TrackName
		info.Developer = it.SellerName
		if info.Developer == "" {
			info.Developer = it.ArtistName
		}
		info.Size, _ = strconv.ParseInt(it.FileSizeBytes, 10, 64)
		info.Price, info.PriceText, info.IconURL, info.MinOS = it.Price, it.FormattedPrice, it.Artwork, it.MinimumOS
	}

	app := appstore.App{ID: info.AppID, BundleID: info.BundleID, Name: info.Name}
	err := s.call(&app, func(acc appstore.Account) error {
		if app.ID == 0 {
			// Not in the configured lookup country: ask the account's storefront.
			out, err := s.as.Lookup(appstore.LookupInput{Account: acc, BundleID: bundleID, Platform: platform})
			if err != nil {
				return err
			}
			app = out.App
			info.Name, info.Price = app.Name, app.Price
		}
		out, err := s.as.ListVersions(appstore.ListVersionsInput{Context: ctx, Account: acc, App: app, Platform: platform})
		if err != nil {
			return err
		}
		info.ExternalVersionID = out.LatestExternalVersionID
		meta, err := s.as.GetVersionMetadata(appstore.GetVersionMetadataInput{
			Context: ctx, Account: acc, App: app, VersionID: out.LatestExternalVersionID, Platform: platform,
		})
		if errors.Is(err, appstore.ErrPasswordTokenExpired) {
			return err
		}
		if err == nil {
			info.Version, info.ReleaseDate = meta.DisplayVersion, meta.ReleaseDate
		}
		return nil
	})
	if err != nil {
		return AppInfo{}, err
	}
	info.AppID, info.BundleID = app.ID, app.BundleID
	if info.Name == "" {
		info.Name = app.Name
	}
	info.Key = fmt.Sprintf("%d:%s", app.ID, info.ExternalVersionID)
	return info, nil
}

// AccountEmail reports which Apple Account the ipatool session uses.
func (s *Store) AccountEmail() (string, error) {
	acc, err := s.account()
	return acc.Email, err
}
