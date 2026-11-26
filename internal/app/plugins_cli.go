package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

const pluginsUsageText = `Usage:
  meldra plugins [list] [--json]
  meldra plugins inspect <name|path> [--json]
  meldra plugins enable <name|path> [--json]
  meldra plugins disable <name> [--purge-data]
  meldra plugins allow-mcp <name> [--json]
  meldra plugins revoke-mcp <name>

List discovers packages in $MELDRA_HOME/plugins/. Enable by manifest name,
or use an explicit path such as ./my-plugin. Enable never grants MCP approval.
`

type pluginListItem struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Path        string   `json:"path"`
	Enabled     bool     `json:"enabled"`
	SkillsCount int      `json:"skills_count"`
	MCPCount    int      `json:"mcp_count"`
	MCPStatus   string   `json:"mcp_status"`
	Extensions  []string `json:"extensions,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type pluginListOutput struct {
	Plugins  []pluginListItem `json:"plugins"`
	Warnings []string         `json:"warnings"`
}

type pluginEnableOutput struct {
	Name        string            `json:"name"`
	Version     string            `json:"version,omitempty"`
	Root        string            `json:"root"`
	DataDir     string            `json:"data_dir"`
	SkillsCount int               `json:"skills_count"`
	MCPCount    int               `json:"mcp_count"`
	MCPStatus   string            `json:"mcp_status"`
	MCPServers  []pluginMCPServer `json:"mcp_servers,omitempty"`
	Warnings    []string          `json:"warnings"`
}

func checkPluginRootAllowed(paths ConfigPaths, canonicalRoot string) error {
	meldraHome, err := canonicalSkillHome(paths.Home)
	if err != nil {
		return err
	}
	allowedPluginsDir := filepath.Join(meldraHome, "plugins")
	if within(canonicalRoot, meldraHome) || withinFold(canonicalRoot, meldraHome) || (withinFold(meldraHome, canonicalRoot) && !within(allowedPluginsDir, canonicalRoot)) {
		return fmt.Errorf("plugin directory cannot overlap Meldra private state %s (place local plugins under %s or outside Meldra home)", meldraHome, allowedPluginsDir)
	}
	return nil
}

func pluginMCPStatus(ctx context.Context, entry pluginRegistryEntry, loaded *loadedPlugin) string {
	if loaded == nil || loaded.MCPDisabled || len(loaded.MCPServers) == 0 {
		return "none"
	}
	if !entry.MCPApproved {
		return "not-approved"
	}
	currentDigest, err := reviewPluginMCP(ctx, loaded)
	if err != nil || entry.MCPDigest == "" || currentDigest != entry.MCPDigest {
		return "approval-stale"
	}
	return "approved"
}

func runPluginsCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return runPluginsList(ctx, nil, output)
	}
	switch args[0] {
	case "help", "--help", "-h":
		_, err := fmt.Fprint(output, pluginsUsageText)
		return err
	case "list":
		return runPluginsList(ctx, args[1:], output)
	case "inspect":
		return runPluginsInspect(ctx, args[1:], output)
	case "enable":
		return runPluginsEnable(ctx, args[1:], output)
	case "disable":
		return runPluginsDisable(ctx, args[1:], output)
	case "allow-mcp":
		return runPluginsAllowMCP(ctx, args[1:], output)
	case "revoke-mcp":
		return runPluginsRevokeMCP(ctx, args[1:], output)
	default:
		if strings.HasPrefix(args[0], "-") {
			return runPluginsList(ctx, args, output)
		}
		return fmt.Errorf("unknown plugins subcommand %q\n\n%s", args[0], pluginsUsageText)
	}
}

func runPluginsList(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("plugins list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(output, pluginsUsageText)
			return writeErr
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plugins list accepts only --json")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	catalog, warnings, err := localPluginCatalog(ctx, paths)
	if err != nil {
		return err
	}
	result := pluginListOutput{Plugins: []pluginListItem{}, Warnings: warnings}
	for _, name := range slices.Sorted(maps.Keys(catalog)) {
		entries := catalog[name]
		if len(entries) > 1 {
			result.Warnings = append(result.Warnings, fmt.Sprintf("multiple packages named %q; use an explicit path", name))
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := pluginListItem{
				Name:    name,
				Path:    entry.Path,
				Enabled: entry.Enabled,
			}
			loaded, loadErr := loadPluginAtRoot(ctx, paths, entry.Path, func(candidate string) error { return checkPluginRootAllowed(paths, candidate) })
			if loadErr != nil {
				item.Error = loadErr.Error()
				item.MCPStatus = "unavailable"
				result.Plugins = append(result.Plugins, item)
				continue
			}
			item.Version = loaded.Manifest.Version
			item.Description = loaded.Manifest.Description
			item.SkillsCount = len(loaded.Skills)
			item.MCPStatus = pluginMCPStatus(ctx, entry, loaded)
			item.MCPCount = len(loaded.MCPServers)
			item.Extensions = loaded.Manifest.Extensions
			item.Warnings = loaded.Warnings
			if loaded.Manifest.Name != name {
				item.Warnings = append(item.Warnings, fmt.Sprintf("manifest name %q differs from registered name %q", loaded.Manifest.Name, name))
			}
			result.Plugins = append(result.Plugins, item)
		}
	}
	if *asJSON {
		return json.NewEncoder(output).Encode(result)
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintf(output, "Warning: %s\n", skillDisplayLine(warning)); err != nil {
			return err
		}
	}
	if len(result.Plugins) == 0 {
		_, err := fmt.Fprintln(output, "No plugins configured.")
		return err
	}
	for _, item := range result.Plugins {
		header := item.Name
		if item.Version != "" {
			header += " " + skillDisplayLine(item.Version)
		}
		if item.Enabled {
			header += " [enabled]"
		} else {
			header += " [available]"
		}
		if item.Error != "" {
			if _, err := fmt.Fprintf(output, "%s\terror: %s\n  %s\n", header, skillDisplayLine(item.Error), skillDisplayLine(item.Path)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(output, "%s\tskills=%d mcp=%d (%s)\n  %s\n", header, item.SkillsCount, item.MCPCount, item.MCPStatus, skillDisplayLine(item.Path)); err != nil {
			return err
		}
		for _, warning := range item.Warnings {
			if _, err := fmt.Fprintf(output, "  Warning: %s\n", skillDisplayLine(warning)); err != nil {
				return err
			}
		}
	}
	return nil
}

func runPluginsInspect(ctx context.Context, args []string, output io.Writer) error {
	var path string
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			_, err := fmt.Fprintln(output, "Usage: meldra plugins inspect <name|path> [--json]")
			return err
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown plugins inspect flag %q", arg)
			}
			if path != "" {
				return fmt.Errorf("plugins inspect accepts one plugin name or path")
			}
			path = arg
		}
	}
	if path == "" {
		return fmt.Errorf("plugins inspect requires a plugin name or path")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	loaded, err := loadPluginArgument(ctx, paths, path, nil)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(output).Encode(loaded)
	}
	if _, err := fmt.Fprintf(output, "Plugin: %s", loaded.Manifest.Name); err != nil {
		return err
	}
	if loaded.Manifest.Version != "" {
		if _, err := fmt.Fprintf(output, " (%s)", skillDisplayLine(loaded.Manifest.Version)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(output, "\nRoot: %s\nData: %s\n", skillDisplayLine(loaded.Root), skillDisplayLine(loaded.DataDir)); err != nil {
		return err
	}
	if loaded.Manifest.Description != "" {
		if _, err := fmt.Fprintf(output, "Description: %s\n", skillDisplayLine(loaded.Manifest.Description)); err != nil {
			return err
		}
	}
	if len(loaded.Manifest.Extensions) > 0 {
		if _, err := fmt.Fprintf(output, "Extensions (ignored): %s\n", skillDisplayLine(strings.Join(loaded.Manifest.Extensions, ", "))); err != nil {
			return err
		}
	}
	if loaded.SkillsDisabled {
		if _, err := fmt.Fprintln(output, "Skills: disabled (invalid location)"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(output, "Skills (%d):\n", len(loaded.Skills)); err != nil {
			return err
		}
		for _, item := range loaded.Skills {
			if _, err := fmt.Fprintf(output, "  %s\t%s\n", item.Name, skillDisplayLine(item.Description)); err != nil {
				return err
			}
		}
	}
	if loaded.MCPDisabled {
		if _, err := fmt.Fprintln(output, "MCP servers: disabled (invalid mcp.json)"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(output, "MCP servers (%d):\n", len(loaded.MCPServers)); err != nil {
			return err
		}
		for _, server := range loaded.MCPServers {
			if err := writePluginMCPServerSummary(output, server); err != nil {
				return err
			}
		}
	}
	for _, warning := range loaded.Warnings {
		if _, err := fmt.Fprintf(output, "Warning: %s\n", skillDisplayLine(warning)); err != nil {
			return err
		}
	}
	return nil
}

func writePluginMCPServerSummary(output io.Writer, server pluginMCPServer) error {
	if server.Type == "stdio" {
		_, err := fmt.Fprintf(output, "  %s [%s] (%s)\n    command: %s\n    cwd: %s\n",
			skillDisplayLine(server.Name), server.Type, skillDisplayLine(server.ServerID),
			skillDisplayLine(server.Command), skillDisplayLine(server.CWD))
		if err != nil {
			return err
		}
		if len(server.Args) > 0 {
			if _, err := fmt.Fprintf(output, "    args: %s\n", skillDisplayLine(strings.Join(server.Args, " "))); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintf(output, "  %s [%s] (%s)\n    url: %s\n",
		skillDisplayLine(server.Name), server.Type, skillDisplayLine(server.ServerID),
		skillDisplayLine(server.URL))
	return err
}

func runPluginsEnable(ctx context.Context, args []string, output io.Writer) error {
	var path string
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			_, err := fmt.Fprintln(output, "Usage: meldra plugins enable <name|path> [--json]")
			return err
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown plugins enable flag %q", arg)
			}
			if path != "" {
				return fmt.Errorf("plugins enable accepts one plugin name or path")
			}
			path = arg
		}
	}
	if path == "" {
		return fmt.Errorf("plugins enable requires a plugin name or path")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	loaded, err := loadPluginArgument(ctx, paths, path, func(candidate string) error {
		return checkPluginRootAllowed(paths, candidate)
	})
	if err != nil {
		return err
	}
	reg, unlock, err := lockPluginRegistry(ctx, paths)
	if err != nil {
		return err
	}
	defer unlock()
	if existing, exists := reg.Plugins[loaded.Manifest.Name]; exists && existing.Path != loaded.Root {
		return fmt.Errorf("plugin %q is already registered from %s; run \"meldra plugins disable %s\" first", loaded.Manifest.Name, existing.Path, loaded.Manifest.Name)
	}
	if _, exists := reg.Plugins[loaded.Manifest.Name]; !exists && len(reg.Plugins) >= maxRegisteredPlugins {
		return fmt.Errorf("plugins configuration exceeds %d plugins", maxRegisteredPlugins)
	}
	entry := reg.Plugins[loaded.Manifest.Name]
	if !entry.Enabled {
		entry = pluginRegistryEntry{Path: loaded.Root, Enabled: true}
	}
	status := pluginMCPStatus(ctx, entry, loaded)
	reg.Plugins[loaded.Manifest.Name] = entry
	if err := savePluginRegistry(paths, reg); err != nil {
		return err
	}
	summary := pluginEnableOutput{
		Name:        loaded.Manifest.Name,
		Version:     loaded.Manifest.Version,
		Root:        loaded.Root,
		DataDir:     loaded.DataDir,
		SkillsCount: len(loaded.Skills),
		MCPCount:    len(loaded.MCPServers),
		MCPStatus:   status,
		MCPServers:  loaded.MCPServers,
		Warnings:    loaded.Warnings,
	}
	if asJSON {
		return json.NewEncoder(output).Encode(summary)
	}
	if _, err := fmt.Fprintf(output, "Enabled plugin %s (%s): skills=%d, mcp=%d (%s)\n",
		summary.Name, skillDisplayLine(summary.Root), summary.SkillsCount, summary.MCPCount, summary.MCPStatus); err != nil {
		return err
	}
	if status == "not-approved" {
		if _, err := fmt.Fprintf(output, "MCP servers are not enabled by default. Review with \"meldra plugins inspect %s\" and run \"meldra plugins allow-mcp %s\" to allow starting them.\n",
			skillDisplayLine(summary.Root), summary.Name); err != nil {
			return err
		}
	}
	for _, warning := range summary.Warnings {
		if _, err := fmt.Fprintf(output, "Warning: %s\n", skillDisplayLine(warning)); err != nil {
			return err
		}
	}
	return nil
}

func runPluginsDisable(ctx context.Context, args []string, output io.Writer) error {
	var name string
	purgeData := false
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			_, err := fmt.Fprintln(output, "Usage: meldra plugins disable <name> [--purge-data]")
			return err
		case "--purge-data":
			purgeData = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown plugins disable flag %q", arg)
			}
			if name != "" {
				return fmt.Errorf("plugins disable accepts one plugin name")
			}
			name = arg
		}
	}
	if err := validatePluginName(name); err != nil {
		return fmt.Errorf("plugins disable requires a valid plugin name")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	reg, unlock, err := lockPluginRegistry(ctx, paths)
	if err != nil {
		return err
	}
	defer unlock()
	if _, exists := reg.Plugins[name]; !exists {
		return fmt.Errorf("plugin %q is not configured", name)
	}
	if purgeData {
		dataDir, err := pluginDataPath(paths, name)
		if err != nil {
			return err
		}
		root, err := openSkillDirectory(filepath.Dir(dataDir))
		if err == nil {
			defer root.Close()
			if err := root.RemoveAll(name); err != nil {
				return fmt.Errorf("remove plugin data directory: %w", err)
			}
			if err := syncDirectory(root.Name()); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	delete(reg.Plugins, name)
	if err := savePluginRegistry(paths, reg); err != nil {
		return err
	}
	if purgeData {
		_, err = fmt.Fprintf(output, "Disabled plugin %s and removed its data directory.\n", name)
		return err
	}
	_, err = fmt.Fprintf(output, "Disabled plugin %s (preserved PLUGIN_DATA).\n", name)
	return err
}

func runPluginsAllowMCP(ctx context.Context, args []string, output io.Writer) error {
	var name string
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			_, err := fmt.Fprintln(output, "Usage: meldra plugins allow-mcp <name> [--json]")
			return err
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown plugins allow-mcp flag %q", arg)
			}
			if name != "" {
				return fmt.Errorf("plugins allow-mcp accepts one plugin name")
			}
			name = arg
		}
	}
	if err := validatePluginName(name); err != nil {
		return fmt.Errorf("plugins allow-mcp requires a valid plugin name")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	reg, unlock, err := lockPluginRegistry(ctx, paths)
	if err != nil {
		return err
	}
	defer unlock()
	entry, exists := reg.Plugins[name]
	if !exists || !entry.Enabled {
		return fmt.Errorf("plugin %q is not enabled", name)
	}
	loaded, err := loadPluginAtRoot(ctx, paths, entry.Path, func(candidate string) error {
		return checkPluginRootAllowed(paths, candidate)
	})
	if err != nil {
		return err
	}
	if loaded.Manifest.Name != name {
		return fmt.Errorf("plugin %q manifest name changed to %q; disable and re-enable it", name, loaded.Manifest.Name)
	}
	if loaded.MCPDisabled || len(loaded.MCPServers) == 0 {
		return fmt.Errorf("plugin %q has no valid MCP servers to approve", name)
	}
	mcpDigest, err := reviewPluginMCP(ctx, loaded)
	if err != nil {
		return err
	}
	entry.MCPApproved = true
	entry.MCPDigest = mcpDigest
	reg.Plugins[name] = entry
	if err := savePluginRegistry(paths, reg); err != nil {
		return err
	}
	summary := pluginEnableOutput{
		Name:        loaded.Manifest.Name,
		Version:     loaded.Manifest.Version,
		Root:        loaded.Root,
		DataDir:     loaded.DataDir,
		SkillsCount: len(loaded.Skills),
		MCPCount:    len(loaded.MCPServers),
		MCPStatus:   "approved",
		MCPServers:  loaded.MCPServers,
		Warnings:    loaded.Warnings,
	}
	if asJSON {
		return json.NewEncoder(output).Encode(summary)
	}
	if _, err := fmt.Fprintf(output, "Approved %d MCP server(s) for plugin %s:\n", len(loaded.MCPServers), name); err != nil {
		return err
	}
	for _, server := range loaded.MCPServers {
		if err := writePluginMCPServerSummary(output, server); err != nil {
			return err
		}
	}
	for _, warning := range loaded.Warnings {
		if _, err := fmt.Fprintf(output, "Warning: %s\n", skillDisplayLine(warning)); err != nil {
			return err
		}
	}
	return nil
}

func runPluginsRevokeMCP(ctx context.Context, args []string, output io.Writer) error {
	var name string
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			_, err := fmt.Fprintln(output, "Usage: meldra plugins revoke-mcp <name>")
			return err
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown plugins revoke-mcp flag %q", arg)
			}
			if name != "" {
				return fmt.Errorf("plugins revoke-mcp accepts one plugin name")
			}
			name = arg
		}
	}
	if err := validatePluginName(name); err != nil {
		return fmt.Errorf("plugins revoke-mcp requires a valid plugin name")
	}
	paths, err := ResolveConfigPaths()
	if err != nil {
		return err
	}
	reg, unlock, err := lockPluginRegistry(ctx, paths)
	if err != nil {
		return err
	}
	defer unlock()
	entry, exists := reg.Plugins[name]
	if !exists {
		return fmt.Errorf("plugin %q is not configured", name)
	}
	entry.MCPApproved = false
	entry.MCPDigest = ""
	reg.Plugins[name] = entry
	if err := savePluginRegistry(paths, reg); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Revoked MCP approval for plugin %s.\n", name)
	return err
}
