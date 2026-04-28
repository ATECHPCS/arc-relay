// arc-sync skill subcommands. Mirrors cmd/arc-sync/main.go's runMemory shape:
// a top-level dispatcher that picks a subcommand handler based on os.Args[2].
package main

import (
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

func runSkill() {
	if len(os.Args) < 3 {
		printSkillUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "list":
		runSkillList()
	case "install":
		runSkillInstall()
	case "remove", "rm", "uninstall":
		runSkillRemove()
	case "sync":
		runSkillSync()
	case "push":
		runSkillPush()
	case "--help", "-h", "help":
		printSkillUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand: %s\n", os.Args[2])
		printSkillUsage()
		os.Exit(1)
	}
}

func printSkillUsage() {
	fmt.Println(`Usage: arc-sync skill <command> [args]

Commands:
  list [--installed|--remote|--assigned] [--json]
                        Show skills. --installed: local only (default).
                        --remote: full relay catalog. --assigned: relay's view of
                        what you should have installed.
  install <slug> [--version VERSION]
                        Pull a skill from the relay and install it under
                        ~/.claude/skills/<slug>/. Defaults to the latest version.
  remove <slug>         Remove an arc-sync-managed skill. Hand-installed skill
                        directories are refused (no .arc-sync-version marker).
  sync [--dry-run]      Reconcile ~/.claude/skills/ against the relay's assigned
                        list: install missing skills, update outdated ones,
                        remove skills no longer assigned. --dry-run prints
                        actions without performing them.
  push <dir> [--version V] [--visibility public|restricted]
                        Admin-only: package <dir> as a tar.gz and upload.
                        <dir> must contain SKILL.md at its root.

Skills install to ~/.claude/skills/<slug>/. arc-sync only touches directories
it created (those carrying a .arc-sync-version marker file); manually-installed
skills are left alone during sync.`)
}

func newSkillManager() *sync.SkillManager {
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
	skillsDir, err := sync.DefaultSkillsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return &sync.SkillManager{
		Client: &relay.Client{
			BaseURL:    strings.TrimRight(creds.RelayURL, "/"),
			APIKey:     creds.APIKey,
			HTTPClient: &http.Client{Timeout: 60 * time.Second},
		},
		SkillsDir: skillsDir,
	}
}

func runSkillList() {
	args := os.Args[3:]
	jsonOut := hasFlagInArgs(args, "--json")
	mode := "installed"
	switch {
	case hasFlagInArgs(args, "--remote"):
		mode = "remote"
	case hasFlagInArgs(args, "--assigned"):
		mode = "assigned"
	case hasFlagInArgs(args, "--installed"):
		mode = "installed"
	}
	mgr := newSkillManager()

	switch mode {
	case "installed":
		rows, err := mgr.ListInstalled()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(rows)
			return
		}
		if len(rows) == 0 {
			fmt.Println("No skills installed in ~/.claude/skills/.")
			return
		}
		fmt.Printf("%-32s  %-10s  %s\n", "SLUG", "VERSION", "STATUS")
		for _, r := range rows {
			status := "managed"
			if !r.Managed {
				status = "hand-installed (not arc-sync managed)"
			}
			version := r.Version
			if version == "" {
				version = "—"
			}
			fmt.Printf("%-32s  %-10s  %s\n", r.Slug, version, status)
		}
	case "remote":
		skills, err := mgr.Client.ListSkills()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(skills)
			return
		}
		if len(skills) == 0 {
			fmt.Println("No skills published on the relay yet.")
			return
		}
		fmt.Printf("%-32s  %-10s  %-12s  %s\n", "SLUG", "VERSION", "VISIBILITY", "STATUS")
		for _, s := range skills {
			status := "active"
			if s.YankedAt != nil {
				status = "yanked"
			}
			ver := s.LatestVersion
			if ver == "" {
				ver = "—"
			}
			fmt.Printf("%-32s  %-10s  %-12s  %s\n", s.Slug, ver, s.Visibility, status)
		}
	case "assigned":
		assigned, err := mgr.Client.ListAssignedSkills()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(assigned)
			return
		}
		if len(assigned) == 0 {
			fmt.Println("Relay reports no skills assigned to you.")
			return
		}
		fmt.Printf("%-32s  %-10s  %-12s\n", "SLUG", "VERSION", "VISIBILITY")
		for _, a := range assigned {
			ver := a.Skill.LatestVersion
			if a.PinnedVersion != nil && *a.PinnedVersion != "" {
				ver = *a.PinnedVersion + " (pinned)"
			}
			fmt.Printf("%-32s  %-10s  %-12s\n", a.Skill.Slug, ver, a.Skill.Visibility)
		}
	}
}

func runSkillInstall() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill install <slug> [--version VERSION]")
		os.Exit(1)
	}
	slug := args[0]
	version := getFlagValue(args[1:], "--version")
	mgr := newSkillManager()

	if version == "" {
		// Resolve "latest" via the relay so we can record the concrete version
		// in the marker. Doing this client-side keeps the relay's redirect
		// surface area smaller (no /api/skills/{slug}/versions/latest endpoint).
		detail, err := mgr.Client.GetSkill(slug)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve skill:", err)
			os.Exit(1)
		}
		if detail == nil {
			fmt.Fprintf(os.Stderr, "skill %q not found on relay\n", slug)
			os.Exit(1)
		}
		if detail.Skill.LatestVersion == "" {
			fmt.Fprintf(os.Stderr, "skill %q has no published versions\n", slug)
			os.Exit(1)
		}
		version = detail.Skill.LatestVersion
	}

	marker, err := mgr.Install(slug, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill install:", err)
		os.Exit(1)
	}
	fmt.Printf("Installed %s@%s into %s/%s/\n", marker.Slug, marker.Version, mgr.SkillsDir, marker.Slug)
}

func runSkillRemove() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill remove <slug>")
		os.Exit(1)
	}
	slug := args[0]
	mgr := newSkillManager()
	if err := mgr.Remove(slug); err != nil {
		fmt.Fprintln(os.Stderr, "skill remove:", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %s from %s/.\n", slug, mgr.SkillsDir)
}

func runSkillSync() {
	args := os.Args[3:]
	dryRun := hasFlagInArgs(args, "--dry-run")
	jsonOut := hasFlagInArgs(args, "--json")
	mgr := newSkillManager()

	report, err := mgr.Sync(sync.SkillSyncOptions{DryRun: dryRun})
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill sync:", err)
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
		fmt.Printf("%sinstall %s@%s\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.Updated {
		fmt.Printf("%supdate  %s: %s → %s\n", prefix, a.Slug, a.Previous, a.Version)
	}
	for _, a := range report.Removed {
		fmt.Printf("%sremove  %s (was %s)\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.Unchanged {
		fmt.Printf("%sok      %s@%s\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.SkippedHand {
		fmt.Printf("%sskip    %s (hand-installed; not arc-sync-managed)\n", prefix, a.Slug)
	}
	if len(report.Errors) > 0 {
		fmt.Println()
		for _, e := range report.Errors {
			fmt.Fprintf(os.Stderr, "error %s: %s\n", e.Slug, e.Message)
		}
		os.Exit(1)
	}
	if len(report.Installed)+len(report.Updated)+len(report.Removed) == 0 && !dryRun {
		fmt.Println("Nothing to do — already in sync.")
	}
}

func runSkillPush() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill push <dir> [--version V] [--visibility public|restricted]")
		os.Exit(1)
	}
	dir := args[0]
	version := getFlagValue(args[1:], "--version")
	visibility := getFlagValue(args[1:], "--visibility")
	if version == "" {
		fmt.Fprintln(os.Stderr, "skill push: --version is required (semver MAJOR.MINOR.PATCH)")
		os.Exit(1)
	}

	archive, slug, err := sync.PackageSkill(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill push:", err)
		os.Exit(1)
	}
	mgr := newSkillManager()
	res, err := mgr.Client.UploadSkill(slug, version, visibility, archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill push:", err)
		os.Exit(1)
	}
	fmt.Printf("Published %s@%s (%d bytes, sha256=%s)\n",
		res.Skill.Slug, res.Version.Version, res.Version.ArchiveSize, res.Version.ArchiveSHA256)
}

// emitJSON marshals v as pretty JSON and writes it to stdout. Used by the
// --json flag on each subcommand. Errors are fatal — the user asked for JSON,
// returning text would be a contract violation.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "encode JSON:", err)
		os.Exit(1)
	}
}
