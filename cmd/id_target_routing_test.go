package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveIDTargetKind(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    idTargetKind
		wantErr bool
	}{
		{name: "job prefixed", args: []string{"wj123"}, want: idTargetJob},
		{name: "instance prefixed", args: []string{"wi123"}, want: idTargetInstance},
		{name: "job prefixed mixed numeric", args: []string{"wj123", "456"}, want: idTargetJob},
		{name: "instance prefixed mixed numeric", args: []string{"wi123", "456"}, want: idTargetInstance},
		{name: "job range with prefix", args: []string{"wj10:12"}, want: idTargetJob},
		{name: "ambiguous numeric only", args: []string{"123"}, wantErr: true},
		{name: "mixed explicit prefixes", args: []string{"wi123", "wj456"}, wantErr: true},
		{name: "mixed explicit prefixes in one token", args: []string{"wi123,wj456"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveIDTargetKind(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveIDTargetKind(%v) expected error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveIDTargetKind(%v) error = %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("resolveIDTargetKind(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunInfoRoutesToInstanceStatus(t *testing.T) {
	origInst := runInstanceStatusFromInfoFunc
	origJob := runJobInfoFromInfoFunc
	t.Cleanup(func() {
		runInstanceStatusFromInfoFunc = origInst
		runJobInfoFromInfoFunc = origJob
	})

	calledInst := false
	calledJob := false
	wantErr := errors.New("instance path")

	runInstanceStatusFromInfoFunc = func(_ *cobra.Command, args []string) error {
		calledInst = true
		if len(args) != 2 || args[0] != "wi123" || args[1] != "456" {
			t.Fatalf("unexpected args: %v", args)
		}
		return wantErr
	}
	runJobInfoFromInfoFunc = func(_ *cobra.Command, _ []string) error {
		calledJob = true
		return nil
	}

	err := runInfo(&cobra.Command{}, []string{"wi123", "456"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runInfo() error = %v, want %v", err, wantErr)
	}
	if !calledInst {
		t.Fatal("expected instance info path to be called")
	}
	if calledJob {
		t.Fatal("did not expect job info path to be called")
	}
}

func TestRunStatusTopLevelRoutesToInstanceStatus(t *testing.T) {
	origInst := runInstanceStatusFromStatusFunc
	t.Cleanup(func() {
		runInstanceStatusFromStatusFunc = origInst
	})

	called := false
	wantErr := errors.New("instance status path")
	runInstanceStatusFromStatusFunc = func(_ *cobra.Command, args []string) error {
		called = true
		if len(args) != 2 || args[0] != "wi123" || args[1] != "456" {
			t.Fatalf("unexpected args: %v", args)
		}
		return wantErr
	}

	err := runStatus(statusCmd, []string{"wi123", "456"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runStatus() error = %v, want %v", err, wantErr)
	}
	if !called {
		t.Fatal("expected instance status path to be called")
	}
}

func TestRunTerminateRoutesByPrefix(t *testing.T) {
	origInst := runInstanceTerminateFunc
	origJob := runJobTerminateFunc
	t.Cleanup(func() {
		runInstanceTerminateFunc = origInst
		runJobTerminateFunc = origJob
	})

	t.Run("instance route", func(t *testing.T) {
		calledInst := false
		calledJob := false
		wantErr := errors.New("instance terminate path")

		runInstanceTerminateFunc = func(_ *cobra.Command, args []string) error {
			calledInst = true
			if len(args) != 2 || args[0] != "wi123" || args[1] != "456" {
				t.Fatalf("unexpected args: %v", args)
			}
			return wantErr
		}
		runJobTerminateFunc = func(_ *cobra.Command, _ []string) error {
			calledJob = true
			return nil
		}

		err := runTerminate(&cobra.Command{}, []string{"wi123", "456"})
		if !errors.Is(err, wantErr) {
			t.Fatalf("runTerminate() error = %v, want %v", err, wantErr)
		}
		if !calledInst {
			t.Fatal("expected instance terminate path to be called")
		}
		if calledJob {
			t.Fatal("did not expect job terminate path to be called")
		}
	})

	t.Run("job route", func(t *testing.T) {
		calledInst := false
		calledJob := false
		wantErr := errors.New("job terminate path")

		runInstanceTerminateFunc = func(_ *cobra.Command, _ []string) error {
			calledInst = true
			return nil
		}
		runJobTerminateFunc = func(_ *cobra.Command, args []string) error {
			calledJob = true
			if len(args) != 2 || args[0] != "wj123" || args[1] != "456" {
				t.Fatalf("unexpected args: %v", args)
			}
			return wantErr
		}

		err := runTerminate(&cobra.Command{}, []string{"wj123", "456"})
		if !errors.Is(err, wantErr) {
			t.Fatalf("runTerminate() error = %v, want %v", err, wantErr)
		}
		if !calledJob {
			t.Fatal("expected job terminate path to be called")
		}
		if calledInst {
			t.Fatal("did not expect instance terminate path to be called")
		}
	})
}
