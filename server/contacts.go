package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
)

// maxContacts keeps one account from growing the file without bound.
const maxContacts = 500

// Contacts is each person's contact list (usernames), kept in a JSON file so
// it survives restarts and follows them across devices.
type Contacts struct {
	mu    sync.Mutex
	path  string
	lists map[string][]string
}

func LoadContacts(path string) (*Contacts, error) {
	c := &Contacts{path: path, lists: map[string][]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &c.lists); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// List returns owner's contacts, sorted.
func (c *Contacts) List(owner string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.lists[owner])
}

// Add puts username on owner's list (no-op if it's already there).
func (c *Contacts) Add(owner, username string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.lists[owner]
	if slices.Contains(list, username) {
		return nil
	}
	if len(list) >= maxContacts {
		return fmt.Errorf("you can have at most %d contacts", maxContacts)
	}
	list = append(list, username)
	slices.Sort(list)
	c.lists[owner] = list
	return c.saveLocked()
}

// Remove takes username off owner's list.
func (c *Contacts) Remove(owner, username string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := slices.DeleteFunc(c.lists[owner], func(a string) bool { return a == username })
	if len(list) == 0 {
		delete(c.lists, owner)
	} else {
		c.lists[owner] = list
	}
	return c.saveLocked()
}

// saveLocked writes via a temp file + rename, so a crash mid-write can't
// leave a truncated file behind.
func (c *Contacts) saveLocked() error {
	raw, err := json.Marshal(c.lists)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
