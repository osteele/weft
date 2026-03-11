package cmd

import (
	"reflect"
	"testing"
)

func TestParseJobIDs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []int64
		wantErr bool
	}{
		{
			name: "single ID",
			args: []string{"123"},
			want: []int64{123},
		},
		{
			name: "multiple IDs",
			args: []string{"123", "456", "789"},
			want: []int64{123, 456, 789},
		},
		{
			name: "unsorted IDs get sorted",
			args: []string{"789", "123", "456"},
			want: []int64{123, 456, 789},
		},
		{
			name: "simple range",
			args: []string{"1:5"},
			want: []int64{1, 2, 3, 4, 5},
		},
		{
			name: "range with single element",
			args: []string{"42:42"},
			want: []int64{42},
		},
		{
			name: "mixed IDs and ranges",
			args: []string{"100", "200:203", "300"},
			want: []int64{100, 200, 201, 202, 203, 300},
		},
		{
			name: "duplicates are removed",
			args: []string{"123", "123", "456"},
			want: []int64{123, 456},
		},
		{
			name: "duplicates from range overlap",
			args: []string{"1:3", "2:4"},
			want: []int64{1, 2, 3, 4},
		},
		{
			name: "real world example",
			args: []string{"2371", "2373:2375"},
			want: []int64{2371, 2373, 2374, 2375},
		},
		{
			name: "comma separated IDs",
			args: []string{"12,13,14"},
			want: []int64{12, 13, 14},
		},
		{
			name: "ellipsis range",
			args: []string{"12...13"},
			want: []int64{12, 13},
		},
		{
			name: "mixed comma and range syntaxes",
			args: []string{"10,11:12", "13...14", "15::16"},
			want: []int64{10, 11, 12, 13, 14, 15, 16},
		},
		{
			name:    "invalid ID",
			args:    []string{"abc"},
			wantErr: true,
		},
		{
			name:    "invalid range start",
			args:    []string{"abc:123"},
			wantErr: true,
		},
		{
			name:    "invalid range end",
			args:    []string{"123:abc"},
			wantErr: true,
		},
		{
			name:    "reversed range",
			args:    []string{"10:5"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJobIDs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseJobIDs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseJobIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseJobIDArg(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		want    []int64
		wantErr bool
	}{
		{
			name: "single ID",
			arg:  "42",
			want: []int64{42},
		},
		{
			name: "range",
			arg:  "10:15",
			want: []int64{10, 11, 12, 13, 14, 15},
		},
		{
			name: "double colon range",
			arg:  "10::12",
			want: []int64{10, 11, 12},
		},
		{
			name: "ellipsis range",
			arg:  "10...12",
			want: []int64{10, 11, 12},
		},
		{
			name: "comma-separated list",
			arg:  "10,12,14",
			want: []int64{10, 12, 14},
		},
		{
			name: "comma-separated mixed",
			arg:  "10,11:12,14...15",
			want: []int64{10, 11, 12, 14, 15},
		},
		{
			name:    "invalid triple colon",
			arg:     "10:::12",
			wantErr: true,
		},
		{
			name:    "invalid multi-colon",
			arg:     "10:12:14",
			wantErr: true,
		},
		{
			name:    "range too large",
			arg:     "1:2000",
			wantErr: true,
		},
		{
			name:    "invalid empty comma segment",
			arg:     "10,,12",
			wantErr: true,
		},
		{
			name:    "invalid multiple ellipsis",
			arg:     "10...12...14",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJobIDArg(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseJobIDArg() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseJobIDArg() = %v, want %v", got, tt.want)
			}
		})
	}
}
