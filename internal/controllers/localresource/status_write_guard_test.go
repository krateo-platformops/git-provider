package localresource

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every status write in the sync path must go through updateStatusWithRetry.
//
// This is a source guard rather than a behavioural test on purpose. The bug it prevents is not a
// wrong result, it is a MISSED CALL SITE: #17 fixed the status write on the committed path and left
// the identical bare write in the `git.NoErrAlreadyUpToDate` branch a few lines above — same
// function, same failure, caught only in review. A behavioural test would have to mock git deeply
// enough to reach each branch and would still say nothing about a third branch added later.
//
// Losing the conflict on either write strands a LocalResource with external-create-pending set and
// no recorded result. The already-up-to-date branch is the worse of the two: it makes no commit, so
// a later Observe has no footer to recognise the resource by and cannot adopt it.
func TestNoBareStatusUpdateOutsideTheRetryHelper(t *testing.T) {
	const file = "localresource.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}

	helper := regexp.MustCompile(`(?s)func \(e \*external\) updateStatusWithRetry\(.*?\n}`)
	outside := helper.ReplaceAllString(string(src), "")

	var offenders []string
	for i, line := range strings.Split(outside, "\n") {
		if strings.Contains(line, "Status().Update(") {
			offenders = append(offenders, strings.TrimSpace(line)+"  (line ~"+itoa(i+1)+")")
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("bare Status().Update outside updateStatusWithRetry — a lost conflict here strands the\n"+
			"resource with external-create-pending set and no recorded result:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
