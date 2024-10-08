// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/cli/clierror"
	"github.com/cockroachdb/cockroach/pkg/cli/clierrorplus"
	"github.com/cockroachdb/cockroach/pkg/cli/exit"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
)

func TestCLITimeout(t *testing.T) {
	defer leaktest.AfterTest(t)()

	c := NewCLITest(TestCLIParams{T: t})
	defer c.Cleanup()

	// Wrap the meat of the test in a retry loop. Setting a timeout like this is
	// racy as the operation may have succeeded by the time the scheduler gives
	// the timeout a chance to have an effect. We specify --all to include some
	// slower to access virtual tables in the query.
	testutils.SucceedsSoon(t, func() error {
		out, err := c.RunWithCapture("node status 1 --all --timeout 1ms")
		if err != nil {
			t.Fatal(err)
		}

		const exp = `node status 1 --all --timeout 1ms
ERROR: query execution canceled due to statement timeout
SQLSTATE: 57014
`
		if out != exp {
			err := errors.Errorf("unexpected output:\n%q\nwanted:\n%q", out, exp)
			t.Log(err)
			return err
		}
		return nil
	})
}

func TestJunkPositionalArguments(t *testing.T) {
	defer leaktest.AfterTest(t)()

	c := NewCLITest(TestCLIParams{T: t, NoServer: true})
	defer c.Cleanup()

	for i, test := range []string{
		"start",
		"sql",
		"gen man",
		"gen example-data intro",
	} {
		const junk = "junk"
		line := test + " " + junk
		out, err := c.RunWithCapture(line)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		exp := fmt.Sprintf("%s\nERROR: unknown command %q for \"cockroach %s\"\n", line, junk, test)
		if exp != out {
			t.Errorf("%d: expected:\n%s\ngot:\n%s", i, exp, out)
		}
	}
}

func Example_exitcode() {
	c := NewCLITest(TestCLIParams{NoServer: true})
	defer c.Cleanup()

	testCmd := &cobra.Command{
		Use: "test-exit-code",
		RunE: clierrorplus.MaybeDecorateError(
			func(_ *cobra.Command, _ []string) error {
				return clierror.NewError(errors.New("err"), exit.Interrupted())
			}),
	}
	cockroachCmd.AddCommand(testCmd)
	defer cockroachCmd.RemoveCommand(testCmd)

	c.reportExitCode = true
	c.Run("test-exit-code")

	// Output:
	// test-exit-code
	// ERROR: err
	// exit code: 3
}
