// Package store persists q3ctl state atomically with restrictive permissions.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type File[T any] struct{ Path string }

func (f File[T]) Load() (T, error) {
	var value T
	bytes, err := os.ReadFile(f.Path)
	if err != nil {
		return value, err
	}
	return value, json.Unmarshal(bytes, &value)
}

func (f File[T]) Save(value T) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0750); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(f.Path), ".state-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(0640); err == nil {
		_, err = temp.Write(bytes)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, f.Path)
}
