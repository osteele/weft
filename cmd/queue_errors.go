package cmd

import "errors"

var (
	errUnknownDepMode  = errors.New("unknown dependency mode")
	errInvalidDepJobID = errors.New("invalid dependency job ID")
	errSelfDependency  = errors.New("job cannot depend on itself")
	errCrossHostDep    = errors.New("cannot depend on job from different host")
	errFlagConflict    = errors.New("conflicting flags")
)
