// arc-sync recipe subcommands. Setup recipes describe HOW to install
// third-party Claude Code plugins from upstream sources. The relay stores
// recipe metadata; this client shells out to the supported `claude plugin`
// CLI to actually install them on the local machine.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/cli/config"
	"github.com/comma-compliance/arc-relay/internal/cli/relay"
	"github.com/comma-compliance/arc-relay/internal/cli/sync"
)

func runRecipe() {
	if len(os.Args) < 3 {
		printRecipeUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "list":
		runRecipeList()
	case "install":
		runRecipeInstall()
	case "uninstall", "remove", "rm":
		runRecipeUninstall()
	case "sync":
		runRecipeSync()
	case "push":
		runRecipePush()
	case "--help", "-h", "help":
		printRecipeUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown recipe subcommand: %s\n", os.Args[2])
		printRecipeUsage()
		os.Exit(1)
	}
}

func printRecipeUsage() {
	fmt.Println(`Usage: arc-sync recipe <command> [args]

Commands:
  list [--remote|--assigned] [--json]
                        Show recipes. --assigned (default) shows what the
                        relay assigns to your user. --remote shows the full
                        relay catalog (admin sees all incl. yanked).
  install <slug>        Install one recipe — runs:
                          claude plugin marketplace add <marketplace_source>
                          claude plugin install <plugin>
                          claude plugin disable <plugin>   (if recipe.enabled=false)
  uninstall <slug>      Run claude plugin uninstall for the recipe's plugin.
                        Does NOT remove the recipe assignment on the relay.
  sync [--dry-run] [--json]
                        Install every assigned recipe. Idempotent — already-
                        installed plugins are reported as 'unchanged'. Sync
                        is install-only and never auto-uninstalls (claude
                        plugin list does not distinguish recipe-installed
                        from hand-installed plugins).
  push <slug>           Admin-only. Create a new claude_plugin recipe.
                        Required flags:
                          --marketplace SOURCE   github owner/repo, git URL, or local path
                          --plugin NAME@MARKET   plugin id passed to claude plugin install
                        Optional:
                          --display-name TEXT
                          --description TEXT
                          --visibility public|restricted   (default restricted)
                          --enabled / --no-enabled         (default enabled)
                          --sparse path                    (repeatable)

Recipes are mechanically just metadata pointing at upstream sources. The
relay never executes them — install runs on this machine via the supported
'claude plugin' CLI. Sync is opt-in.`)
}

func newRecipeManager() *sync.RecipeManager {
	configDir, err := config.DefaultConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	creds, err := config.ResolveCredentials(configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return &sync.RecipeManager{
		Client: &relay.Client{
			BaseURL:    strings.TrimRight(creds.RelayURL, "/"),
			APIKey:     creds.APIKey,
			HTTPClient: &http.Client{Timeout: 60 * time.Second},
		},
		Runner: &sync.ExecClaudeRunner{},
	}
}

func runRecipeList() {
	args := os.Args[3:]
	jsonOut := hasFlagInArgs(args, "--json")
	mode := "assigned"
	if hasFlagInArgs(args, "--remote") {
		mode = "remote"
	} else if hasFlagInArgs(args, "--assigned") {
		mode = "assigned"
	}
	mgr := newRecipeManager()

	switch mode {
	case "assigned":
		assigned, err := mgr.Client.ListAssignedRecipes()
		if err != nil {
			fmt.Fprintln(os.Stderr, "recipe list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(assigned)
			return
		}
		if len(assigned) == 0 {
			fmt.Println("Relay reports no recipes assigned to you.")
			return
		}
		fmt.Printf("%-32s  %-14s  %-12s  %s\n", "SLUG", "TYPE", "VISIBILITY", "PLUGIN")
		for _, a := range assigned {
			plug := pluginField(a.Recipe.RecipeData)
			fmt.Printf("%-32s  %-14s  %-12s  %s\n", a.Recipe.Slug, a.Recipe.RecipeType,
				a.Recipe.Visibility, plug)
		}
	case "remote":
		recs, err := mgr.Client.ListRecipes()
		if err != nil {
			fmt.Fprintln(os.Stderr, "recipe list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(recs)
			return
		}
		if len(recs) == 0 {
			fmt.Println("No recipes published on the relay yet.")
			return
		}
		fmt.Printf("%-32s  %-14s  %-12s  %-9s  %s\n", "SLUG", "TYPE", "VISIBILITY", "STATUS", "PLUGIN")
		for _, r := range recs {
			status := "active"
			if r.YankedAt != nil {
				status = "yanked"
			}
			fmt.Printf("%-32s  %-14s  %-12s  %-9s  %s\n", r.Slug, r.RecipeType,
				r.Visibility, status, pluginField(r.RecipeData))
		}
	}
}

func runRecipeInstall() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync recipe install <slug>")
		os.Exit(1)
	}
	slug := args[0]
	mgr := newRecipeManager()

	r, err := mgr.Client.GetRecipe(slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch recipe:", err)
		os.Exit(1)
	}
	if r == nil {
		fmt.Fprintf(os.Stderr, "recipe %q not found on relay\n", slug)
		os.Exit(1)
	}
	if err := mgr.Install(context.Background(), r); err != nil {
		fmt.Fprintln(os.Stderr, "recipe install:", err)
		os.Exit(1)
	}
	plug := pluginField(r.RecipeData)
	fmt.Printf("Installed recipe %s (plugin: %s)\n", r.Slug, plug)
}

func runRecipeUninstall() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync recipe uninstall <slug>")
		os.Exit(1)
	}
	slug := args[0]
	mgr := newRecipeManager()

	r, err := mgr.Client.GetRecipe(slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch recipe:", err)
		os.Exit(1)
	}
	if r == nil {
		fmt.Fprintf(os.Stderr, "recipe %q not found on relay\n", slug)
		os.Exit(1)
	}
	if err := mgr.Uninstall(context.Background(), r); err != nil {
		fmt.Fprintln(os.Stderr, "recipe uninstall:", err)
		os.Exit(1)
	}
	fmt.Printf("Uninstalled plugin from recipe %s\n", r.Slug)
}

func runRecipeSync() {
	args := os.Args[3:]
	dryRun := hasFlagInArgs(args, "--dry-run")
	jsonOut := hasFlagInArgs(args, "--json")
	mgr := newRecipeManager()

	report, err := mgr.Sync(context.Background(), sync.RecipeSyncOptions{DryRun: dryRun})
	if err != nil {
		fmt.Fprintln(os.Stderr, "recipe sync:", err)
		os.Exit(1)
	}
	if jsonOut {
		emitJSON(report)
		if len(report.Errors) > 0 {
			os.Exit(1)
		}
		return
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	for _, a := range report.Installed {
		fmt.Printf("%sinstall   %s  (plugin %s from %s)\n", prefix, a.Slug, a.Plugin, a.Marketplace)
	}
	for _, a := range report.Unchanged {
		fmt.Printf("%salready   %s  (plugin %s)\n", prefix, a.Slug, a.Plugin)
	}
	if len(report.Errors) > 0 {
		fmt.Println()
		for _, e := range report.Errors {
			fmt.Fprintf(os.Stderr, "error %s: %s\n", e.Slug, e.Message)
		}
		os.Exit(1)
	}
	if len(report.Installed)+len(report.Unchanged) == 0 {
		fmt.Println("No recipes assigned to you yet.")
	}
}

func runRecipePush() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync recipe push <slug> --marketplace SOURCE --plugin NAME@MARKET [--visibility public|restricted] [--display-name X] [--description X] [--no-enabled] [--sparse PATH ...]")
		os.Exit(1)
	}
	slug := args[0]
	rest := args[1:]

	source := getFlagValue(rest, "--marketplace")
	plugin := getFlagValue(rest, "--plugin")
	if source == "" || plugin == "" {
		fmt.Fprintln(os.Stderr, "--marketplace and --plugin are required")
		os.Exit(1)
	}
	visibility := getFlagValue(rest, "--visibility")
	displayName := getFlagValue(rest, "--display-name")
	description := getFlagValue(rest, "--description")
	enabled := !hasFlagInArgs(rest, "--no-enabled")
	sparse := allFlagValues(rest, "--sparse")

	data, err := json.Marshal(relay.ClaudePluginRecipeData{
		MarketplaceSource: source,
		Plugin:            plugin,
		Enabled:           enabled,
		SparsePaths:       sparse,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal recipe_data:", err)
		os.Exit(1)
	}

	mgr := newRecipeManager()
	res, err := mgr.Client.CreateRecipe(&relay.CreateRecipeRequest{
		Slug:        slug,
		DisplayName: displayName,
		Description: description,
		RecipeType:  "claude_plugin",
		RecipeData:  data,
		Visibility:  visibility,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "recipe push:", err)
		os.Exit(1)
	}
	fmt.Printf("Published recipe %s (plugin: %s, visibility: %s)\n", res.Slug, plugin, res.Visibility)
}

// pluginField extracts the "plugin" field from raw recipe_data JSON for
// rendering. Returns "—" on parse failure rather than failing the whole list.
func pluginField(raw json.RawMessage) string {
	var d relay.ClaudePluginRecipeData
	if err := json.Unmarshal(raw, &d); err != nil {
		return "—"
	}
	if d.Plugin == "" {
		return "—"
	}
	return d.Plugin
}

// allFlagValues collects every occurrence of a repeatable flag (e.g.
// --sparse foo --sparse bar). Used by recipe push for sparse_paths.
func allFlagValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}
