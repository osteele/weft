package cmd

import (
	"reflect"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestDiagnoseCommandSupportsDignoseAlias(t *testing.T) {
	if !slices.Contains(diagnoseCmd.Aliases, "dignose") {
		t.Fatalf("diagnose aliases = %v, want dignose", diagnoseCmd.Aliases)
	}
}

func TestRunDiagnoseRoutesJobForms(t *testing.T) {
	prevJob := runDiagnoseJobFunc
	prevInstance := runDiagnoseInstanceFunc
	t.Cleanup(func() {
		runDiagnoseJobFunc = prevJob
		runDiagnoseInstanceFunc = prevInstance
	})

	var got []string
	runDiagnoseJobFunc = func(_ *cobra.Command, args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	runDiagnoseInstanceFunc = func(_ *cobra.Command, _ []string) error {
		t.Fatal("instance diagnose should not be called")
		return nil
	}

	if err := runDiagnose(&cobra.Command{}, []string{"wj3390"}); err != nil {
		t.Fatalf("runDiagnose wj: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"wj3390"}) {
		t.Fatalf("job args = %v, want [wj3390]", got)
	}

	if err := runDiagnose(&cobra.Command{}, []string{"job", "wj3390"}); err != nil {
		t.Fatalf("runDiagnose job: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"wj3390"}) {
		t.Fatalf("job args = %v, want [wj3390]", got)
	}
}

func TestRunDiagnoseRoutesInstancePrefix(t *testing.T) {
	prevJob := runDiagnoseJobFunc
	prevInstance := runDiagnoseInstanceFunc
	t.Cleanup(func() {
		runDiagnoseJobFunc = prevJob
		runDiagnoseInstanceFunc = prevInstance
	})

	var got []string
	runDiagnoseJobFunc = func(_ *cobra.Command, _ []string) error {
		t.Fatal("job diagnose should not be called")
		return nil
	}
	runDiagnoseInstanceFunc = func(_ *cobra.Command, args []string) error {
		got = append([]string(nil), args...)
		return nil
	}

	if err := runDiagnose(&cobra.Command{}, []string{"wi4084"}); err != nil {
		t.Fatalf("runDiagnose wi: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"wi4084"}) {
		t.Fatalf("instance args = %v, want [wi4084]", got)
	}
}
