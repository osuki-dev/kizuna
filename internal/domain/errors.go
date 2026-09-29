package domain

import "errors"

var (
	ErrUnauthorized    = errors.New("unauthorized: missing or invalid authentication token")
	ErrInvalidPIN      = errors.New("invalid or expired pairing PIN")
	ErrServiceNotFound = errors.New("service not found in project configuration")
	ErrNodeNotFound    = errors.New("target node not found")
	ErrBuildFailed     = errors.New("local build step failed")
	ErrDeployFailed    = errors.New("deployment execution failed")
	ErrInvalidConfig   = errors.New("invalid project configuration file")
)
