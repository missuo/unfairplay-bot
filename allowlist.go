package main

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

type AllowedUser struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name,omitempty"`
	Username string    `json:"username,omitempty"`
	Note     string    `json:"note,omitempty"`
	AddedBy  int64     `json:"added_by"`
	AddedAt  time.Time `json:"added_at"`
}

// Allowlist is the set of non-admin Telegram users allowed to use the bot,
// persisted as JSON in the data directory.
type Allowlist struct {
	mu    sync.Mutex
	path  string
	users map[int64]AllowedUser
}

func OpenAllowlist(path string) (*Allowlist, error) {
	a := &Allowlist{path: path, users: map[int64]AllowedUser{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var list []AllowedUser
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, u := range list {
		a.users[u.ID] = u
	}
	return a, nil
}

func (a *Allowlist) saveLocked() error {
	data, err := json.MarshalIndent(a.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func (a *Allowlist) listLocked() []AllowedUser {
	list := make([]AllowedUser, 0, len(a.users))
	for _, u := range a.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].AddedAt.Before(list[j].AddedAt) })
	return list
}

func (a *Allowlist) Has(id int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.users[id]
	return ok
}

// Allow adds a user; it reports false if the user was already allowed.
func (a *Allowlist) Allow(u AllowedUser) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.users[u.ID]; ok {
		return false, nil
	}
	a.users[u.ID] = u
	return true, a.saveLocked()
}

func (a *Allowlist) Revoke(id int64) (AllowedUser, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return u, false, nil
	}
	delete(a.users, id)
	return u, true, a.saveLocked()
}

// Refresh records the latest display name of an allowed user.
func (a *Allowlist) Refresh(id int64, name, username string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok || (u.Name == name && u.Username == username) {
		return
	}
	u.Name, u.Username = name, username
	a.users[id] = u
	_ = a.saveLocked()
}

func (a *Allowlist) List() []AllowedUser {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listLocked()
}
