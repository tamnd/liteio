// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"sync"
)

// driveResult pairs a per-drive action result with its error.
type driveResult[T any] struct {
	val T
	err error
}

// fanOut runs fn against each of n drives concurrently and returns the per-drive
// results in index order. Each fn receives the drive index. fanOut always waits
// for every drive (callers apply quorum logic to the collected results), so a
// slow drive does not let a caller act on a partial set without noticing it.
func fanOut[T any](ctx context.Context, n int, fn func(ctx context.Context, i int) (T, error)) []driveResult[T] {
	results := make([]driveResult[T], n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			v, err := fn(ctx, i)
			results[i] = driveResult[T]{val: v, err: err}
		}(i)
	}
	wg.Wait()
	return results
}

// countOK returns how many results carry no error.
func countOK[T any](results []driveResult[T]) int {
	n := 0
	for _, r := range results {
		if r.err == nil {
			n++
		}
	}
	return n
}
