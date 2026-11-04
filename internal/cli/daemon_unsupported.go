//go:build !windows && !linux

package cli

import "errors"

func platformServiceManager() (serviceManager, error) {
	return nil, errors.New("daemon management is not supported on this platform; use the documented service example")
}
