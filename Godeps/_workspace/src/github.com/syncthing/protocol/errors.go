// Copyright (C) 2014 The Protocol Authors.

package protocol

import (
	"errors"
)

const (
	ecNoError uint32 = iota
	ecGeneric
	ecNoSuchFile
	ecInvalid
)

var (
	ErrNoError    error = nil
	ErrGeneric          = errors.New("generic error")
	ErrNoSuchFile       = errors.New("no such file")
	ErrInvalid          = errors.New("file is invalid")
)

var lookupError = map[uint32]error{
	ecNoError:    ErrNoError,
	ecGeneric:    ErrGeneric,
	ecNoSuchFile: ErrNoSuchFile,
	ecInvalid:    ErrInvalid,
}

var lookupCode = map[error]uint32{
	ErrNoError:    ecNoError,
	ErrGeneric:    ecGeneric,
	ErrNoSuchFile: ecNoSuchFile,
	ErrInvalid:    ecInvalid,
}

func codeToError(errcode uint32) error {
	err, ok := lookupError[errcode]
	if !ok {
		return ErrGeneric
	}
	return err
}

func errorToCode(err error) uint32 {
	code, ok := lookupCode[err]
	if !ok {
		return ecGeneric
	}
	return code
}
