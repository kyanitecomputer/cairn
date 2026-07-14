package main

import (
	"fmt"
	"strconv"
	"strings"
)

func alignUp(n, alignment uint64) uint64 {
	return (n + alignment - 1) &^ (alignment - 1)
}

func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}
	upper := strings.ToUpper(s)
	multipliers := map[byte]uint64{'K': 1024, 'M': 1024 * 1024, 'G': 1024 * 1024 * 1024}
	last := upper[len(upper)-1]
	if m, ok := multipliers[last]; ok {
		v, err := strconv.ParseUint(upper[:len(upper)-1], 10, 64)
		if err != nil {
			return 0, err
		}
		return v * m, nil
	}
	return strconv.ParseUint(s, 0, 64)
}

func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

type repeatFlag []string

func (f *repeatFlag) String() string { return strings.Join(*f, ", ") }
func (f *repeatFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
