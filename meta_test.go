package main

import (
	"reflect"
	"testing"
)

func TestParseMeta(t *testing.T) {
	contents := `
#!/bin/bash
# on push branch=nope
## on push branch=main
## on push branch=wtflol
## on push branch~=gh-readonly-queue/main/.*
## on pull_request
on alalalalalaaaaa
`
	want := &Meta{
		Events: []MetaEvent{
			{
				Event:      "push",
				Conditions: []DirectiveCondition{{Key: "branch", Op: "=", Value: "main"}},
			},
			{
				Event:      "push",
				Conditions: []DirectiveCondition{{Key: "branch", Op: "=", Value: "wtflol"}},
			},
			{
				Event:      "push",
				Conditions: []DirectiveCondition{{Key: "branch", Op: "~=", Value: "gh-readonly-queue/main/.*"}},
			},
			{
				Event:      "pull_request",
				Conditions: []DirectiveCondition{},
			},
		},
	}

	got, err := parseMeta(contents)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseDirective(t *testing.T) {
	tests := []struct {
		in      string
		want    *Directive
		wantErr bool
	}{
		{
			in: "on push branch=main",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "=", Value: "main"},
				},
			},
		},

		{
			in: "on push branch!=main branch!=foo",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "!=", Value: "main"},
					{Key: "branch", Op: "!=", Value: "foo"},
				},
			}},
		{
			in:      "on push branch=main branch=foo bar",
			wantErr: true,
		},
		{
			in: "on push branch=main foo~=foo bar!~=baz",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "=", Value: "main"},
					{Key: "foo", Op: "~=", Value: "foo"},
					{Key: "bar", Op: "!~=", Value: "baz"},
				},
			},
		},
		{
			in: "on push branch=\"foo\"",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "=", Value: "foo"},
				},
			},
		},
		{
			in: "on push branch=\"\\\"\"",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "=", Value: "\""},
				},
			},
		},
		{
			in: "on push branch=\"\\\\\\n\" branch!=asdf",
			want: &Directive{
				Args: []string{"on", "push"},
				Conditions: []DirectiveCondition{
					{Key: "branch", Op: "=", Value: "\\\n"},
					{Key: "branch", Op: "!=", Value: "asdf"},
				},
			},
		},
	}

	for _, test := range tests {
		got, err := parseDirective(test.in)
		if test.wantErr {
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			continue
		}

		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("got %v, want %v", got, test.want)
		}
	}

}
