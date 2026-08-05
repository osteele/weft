package dataloc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml"
)

// UVPathSource is a [tool.uv.sources] path dependency declared by a project
// or PEP 723 script.
type UVPathSource struct {
	Name   string
	Path   string
	Base   string
	File   string
	Script bool
}

// ScanUVPathSources reads project pyproject.toml and PEP 723 script metadata
// for [tool.uv.sources] entries that use local path dependencies.
func ScanUVPathSources(projectRoot string, commands []string) ([]UVPathSource, error) {
	var out []UVPathSource
	projectSources, err := ScanProjectUVPathSources(projectRoot)
	if err != nil {
		return nil, err
	}
	out = append(out, projectSources...)
	for _, command := range commands {
		scriptSources, err := ScanScriptUVPathSources(projectRoot, command)
		if err != nil {
			return nil, err
		}
		out = append(out, scriptSources...)
	}
	return out, nil
}

// ScanProjectUVPathSources reads [tool.uv.sources] path entries from the
// project's pyproject.toml. Missing pyproject.toml is confirmed absence.
func ScanProjectUVPathSources(projectRoot string) ([]UVPathSource, error) {
	path := filepath.Join(projectRoot, "pyproject.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	tree, err := toml.LoadBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return uvPathSourcesFromTree(tree, filepath.Dir(path), path, false), nil
}

// ScanScriptUVPathSources reads [tool.uv.sources] path entries from PEP 723
// metadata blocks in Python scripts referenced by command.
func ScanScriptUVPathSources(projectRoot, command string) ([]UVPathSource, error) {
	var out []UVPathSource
	for _, script := range ExtractPythonScriptsInDir(projectRoot, command) {
		abs := resolveScriptPath(projectRoot, command, script)
		content, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read script metadata %s: %w", abs, err)
		}
		block := extractPEP723Block(string(content))
		if block == "" {
			continue
		}
		tree, err := toml.Load(block)
		if err != nil {
			return nil, fmt.Errorf("parse script metadata %s: %w", abs, err)
		}
		out = append(out, uvPathSourcesFromTree(tree, filepath.Dir(abs), abs, true)...)
	}
	return out, nil
}

func uvPathSourcesFromTree(tree *toml.Tree, base, file string, script bool) []UVPathSource {
	raw := tree.Get("tool.uv.sources")
	sourcesTree, ok := raw.(*toml.Tree)
	if !ok {
		return nil
	}
	var out []UVPathSource
	for _, name := range sourcesTree.Keys() {
		source, ok := sourcesTree.Get(name).(*toml.Tree)
		if !ok {
			continue
		}
		path, ok := source.Get("path").(string)
		if !ok {
			continue
		}
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		out = append(out, UVPathSource{
			Name:   name,
			Path:   path,
			Base:   base,
			File:   file,
			Script: script,
		})
	}
	return out
}
