package models

import "errors"

// Common errors used throughout the models package
var (
	// MicroVM related errors
	ErrInvalidMicroVMID   = errors.New("invalid microVM ID")
	ErrInvalidNodeID      = errors.New("invalid node ID")
	ErrInvalidCustomerID  = errors.New("invalid customer ID")
	ErrMicroVMNotFound    = errors.New("microVM not found")
	ErrMicroVMNotRunning  = errors.New("microVM is not running")
	ErrMicroVMNotStopped  = errors.New("microVM is not stopped")

	// Resource related errors
	ErrInsufficientResources = errors.New("insufficient resources available")
	ErrInvalidResourceSpecs  = errors.New("invalid resource specifications")
	ErrNodeOverloaded        = errors.New("node is overloaded")
	ErrNodeNotFound          = errors.New("node not found")

	// Storage related errors
	ErrInvalidStorageConfig = errors.New("invalid storage configuration")
	ErrS3ConfigRequired     = errors.New("S3 configuration required for S3 storage types")
	ErrStorageNotAvailable  = errors.New("storage not available")

	// Network related errors
	ErrInvalidNetworkConfig = errors.New("invalid network configuration")
	ErrNetworkNotAvailable  = errors.New("network not available")
	ErrInvalidIPAddress     = errors.New("invalid IP address")

	// Pricing related errors
	ErrInvalidPricingConfig = errors.New("invalid pricing configuration")
	ErrPricingNotAvailable  = errors.New("pricing information not available")
)