//go:build !linux && !darwin

package worker

import "errors"

func bootID() (string, error)              { return "", errors.New("worker: unsupported platform") }
func memoryLoad() (uint64, float64, error) { return 0, 0, errors.New("worker: unsupported platform") }
