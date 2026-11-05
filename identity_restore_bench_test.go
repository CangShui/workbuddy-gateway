package main

import (
	"fmt"
	"strings"
	"testing"
)

var (
	benchmarkRestoreSink string
	benchmarkRestorePlan = func() identityRestore {
		plan := identityRestore{
			rules: []identityRestoreRule{{from: "WorkBuddy", to: "Claude Code"}},
			hold:  len("WorkBuddy"),
		}
		plan.firstBytes['W'] = true
		return plan
	}()
	benchmarkPlain20K  = strings.Repeat("普通正文 without a target. ", 800)
	benchmarkTarget20K = strings.Repeat("WorkBuddy ", 2000)
)

func BenchmarkIdentityRestoreDisabled20K(b *testing.B) {
	b.SetBytes(int64(len(benchmarkPlain20K)))
	for i := 0; i < b.N; i++ {
		restorer := newIdentityRestorer(identityRestore{})
		benchmarkRestoreSink = restorer.Write(benchmarkPlain20K)
	}
}

func BenchmarkIdentityRestorePlain20K(b *testing.B) {
	b.SetBytes(int64(len(benchmarkPlain20K)))
	for i := 0; i < b.N; i++ {
		restorer := newIdentityRestorer(benchmarkRestorePlan)
		benchmarkRestoreSink = restorer.Write(benchmarkPlain20K) + restorer.Flush()
	}
}

func BenchmarkIdentityRestoreTarget20K(b *testing.B) {
	b.SetBytes(int64(len(benchmarkTarget20K)))
	for i := 0; i < b.N; i++ {
		restorer := newIdentityRestorer(benchmarkRestorePlan)
		benchmarkRestoreSink = restorer.Write(benchmarkTarget20K) + restorer.Flush()
	}
}

func BenchmarkIdentityRestoreChunks(b *testing.B) {
	for _, size := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("chunk_%d", size), func(b *testing.B) {
			b.SetBytes(int64(len(benchmarkTarget20K)))
			for i := 0; i < b.N; i++ {
				restorer := newIdentityRestorer(benchmarkRestorePlan)
				var out strings.Builder
				for start := 0; start < len(benchmarkTarget20K); start += size {
					end := start + size
					if end > len(benchmarkTarget20K) {
						end = len(benchmarkTarget20K)
					}
					out.WriteString(restorer.Write(benchmarkTarget20K[start:end]))
				}
				out.WriteString(restorer.Flush())
				benchmarkRestoreSink = out.String()
			}
		})
	}
}
