// Copyright 2014 Canonical Ltd.
// Licensed under the LGPLv3 with static-linking exception.
// See LICENCE file for details.

package ratelimit

import (
	"io"
	"strings"
	"testing"
)

func TestReaderWriter(t *testing.T) {
	rd := Reader(strings.NewReader(strings.Repeat("abcd", 1024)),
		NewBucketWithRate(1024.0, 1024))
	wd := Writer(&strings.Builder{}, NewBucketWithRate(1024.0, 1024))
	n, err := io.Copy(wd, rd)
	if err != nil {
		t.Fatalf("unexpect error(%v)", err)
	}
	if n != 1024*4 {
		t.Fatalf("unexpect eof")
	}
}
