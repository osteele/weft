package db

import "testing"

func TestParseCdCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantCmd    string
		wantDir    string
		wantParsed bool
	}{
		{
			name:       "simple cd with &&",
			command:    "cd /foo/bar && python train.py",
			wantCmd:    "python train.py",
			wantDir:    "/foo/bar",
			wantParsed: true,
		},
		{
			name:       "cd with tilde",
			command:    "cd ~/code/project && make build",
			wantCmd:    "make build",
			wantDir:    "~/code/project",
			wantParsed: true,
		},
		{
			name:       "cd with quoted dir",
			command:    "cd '/path/with spaces' && ./run.sh",
			wantCmd:    "./run.sh",
			wantDir:    "/path/with spaces",
			wantParsed: true,
		},
		{
			name:       "cd with double-quoted dir",
			command:    `cd "/path/with spaces" && ./run.sh`,
			wantCmd:    "./run.sh",
			wantDir:    "/path/with spaces",
			wantParsed: true,
		},
		{
			name:       "no cd prefix",
			command:    "python train.py",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "cd without &&",
			command:    "cd /foo/bar",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "command with && but no cd",
			command:    "make build && make test",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "whitespace handling",
			command:    "  cd /foo/bar  &&  python train.py  ",
			wantCmd:    "python train.py",
			wantDir:    "/foo/bar",
			wantParsed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			gotCmd, gotDir := job.ParseCdCommand()

			if tt.wantParsed {
				if gotCmd != tt.wantCmd {
					t.Errorf("ParseCdCommand() cmd = %q, want %q", gotCmd, tt.wantCmd)
				}
				if gotDir != tt.wantDir {
					t.Errorf("ParseCdCommand() dir = %q, want %q", gotDir, tt.wantDir)
				}
			} else {
				if gotCmd != "" || gotDir != "" {
					t.Errorf("ParseCdCommand() = (%q, %q), want empty strings", gotCmd, gotDir)
				}
			}
		})
	}
}

func TestEffectiveCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "cd pattern returns command after &&",
			command: "cd /foo && python train.py",
			want:    "python train.py",
		},
		{
			name:    "no cd pattern returns original",
			command: "python train.py",
			want:    "python train.py",
		},
		{
			name:    "export prefix is stripped",
			command: "export TMPDIR=/tmp && python train.py",
			want:    "python train.py",
		},
		{
			name:    "multiple export prefixes are stripped",
			command: "export A=1 && export B=2 && python train.py",
			want:    "python train.py",
		},
		{
			name:    "cd then export is stripped",
			command: "cd /foo && export TMPDIR=/tmp && python train.py",
			want:    "python train.py",
		},
		{
			name:    "export without && is preserved",
			command: "export FOO=bar",
			want:    "export FOO=bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			got := job.EffectiveCommand()
			if got != tt.want {
				t.Errorf("EffectiveCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseExportVars(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "no exports",
			command: "python train.py",
			want:    nil,
		},
		{
			name:    "single export",
			command: "export TMPDIR=/tmp && python train.py",
			want:    []string{"TMPDIR=/tmp"},
		},
		{
			name:    "multiple exports",
			command: "export A=1 && export B=2 && python train.py",
			want:    []string{"A=1", "B=2"},
		},
		{
			name:    "cd then export",
			command: "cd /foo && export TMPDIR=/tmp && python train.py",
			want:    []string{"TMPDIR=/tmp"},
		},
		{
			name:    "export without && returns nothing",
			command: "export FOO=bar",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			got := job.ParseExportVars()
			if len(got) != len(tt.want) {
				t.Errorf("ParseExportVars() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseExportVars()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEffectiveWorkingDir(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		workingDir string
		want       string
	}{
		{
			name:       "cd pattern returns dir from cd",
			command:    "cd /foo && python train.py",
			workingDir: "~/original",
			want:       "/foo",
		},
		{
			name:       "no cd pattern returns workingDir",
			command:    "python train.py",
			workingDir: "~/original",
			want:       "~/original",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command, WorkingDir: tt.workingDir}
			got := job.EffectiveWorkingDir()
			if got != tt.want {
				t.Errorf("EffectiveWorkingDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds  int64
		expected string
	}{
		{0, "0s"},
		{1, "1s"},
		{59, "59s"},
		{60, "1m"},
		{61, "1m 1s"},
		{119, "1m 59s"},
		{120, "2m"},
		{3600, "1h"},
		{3601, "1h 1s"},
		{3661, "1h 1m 1s"},
		{7200, "2h"},
		{7325, "2h 2m 5s"},
		{86400, "24h"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := FormatDuration(tt.seconds)
			if got != tt.expected {
				t.Errorf("FormatDuration(%d) = %q, want %q", tt.seconds, got, tt.expected)
			}
		})
	}
}
