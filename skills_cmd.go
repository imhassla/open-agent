package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/imhassla/open-agent/internal/skills"
)

func runSkills(args []string, out, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(out, "usage: open-agent skills list [--verbose] [--json]")
		return 0
	}
	// Inventory flags are booleans, so collect them around the subcommand just
	// as the top-level CLI accepts flags around its verb. Unknown flags are still
	// rejected by the inventory's own parser; model options cannot leak through.
	var positional, options []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			options = append(options, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 || positional[0] != "list" {
		fmt.Fprintln(stderr, "usage: open-agent skills list [--verbose] [--json]")
		return 2
	}
	return runSkillInventory(options, out, stderr, true)
}

// The same read-only command powers both surfaces. Interactive calls exclude
// JSON and report errors without exiting or touching session state.
func runSkillInventory(args []string, out, stderr io.Writer, allowJSON bool) int {
	usage := "/skills [--verbose]"
	if allowJSON {
		usage = "open-agent skills list [--verbose] [--json]"
	}
	flags := flag.NewFlagSet("skills", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, "usage:", usage) }
	verbose := flags.Bool("verbose", false, "show descriptions and full source paths")
	flags.BoolVar(verbose, "v", false, "show descriptions and full source paths")
	jsonOut := false
	if allowJSON {
		flags.BoolVar(&jsonOut, "json", false, "print the full inventory as JSON")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected arguments:", strings.Join(flags.Args(), " "))
		flags.Usage()
		return 2
	}
	inventory := skills.DiscoverCurrent().Inventory()
	var err error
	if jsonOut {
		err = json.NewEncoder(out).Encode(inventory)
	} else {
		_, err = io.WriteString(out, formatSkillInventory(inventory, *verbose))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !inventory.Complete {
		return 1
	}
	return 0
}

func formatSkillInventory(inventory skills.Inventory, verbose bool) string {
	var out strings.Builder
	if !inventory.Complete {
		out.WriteString("Skill inventory incomplete. See Diagnostics below.\n")
	}
	if len(inventory.Skills) == 0 {
		out.WriteString("No skills available.\n")
	} else {
		width := 0
		for _, entry := range inventory.Skills {
			if n := utf8.RuneCountInString(skills.TerminalText(entry.Name)); n > width {
				width = n
			}
		}
		for _, entry := range inventory.Skills {
			policy := ""
			if entry.UserOnly {
				policy = "locked by author · user-only · "
			}
			fmt.Fprintf(&out, "✔ %-*s  %s%s · ~%d tok\n", width, skills.TerminalText(entry.Name), policy, entry.Scope, entry.EstimatedTokens)
			if verbose {
				fmt.Fprintf(&out, "    %s\n    %s\n", skills.TerminalText(entry.Description), skills.TerminalText(entry.Source))
			}
		}
		out.WriteString("\n✔ = available; instructions may not be loaded.\n")
		out.WriteString("~tok estimates name + description metadata, not full instructions, billed tokens or automatic-context usage.\n")
	}
	if len(inventory.Diagnostics) > 0 {
		out.WriteString("\nDiagnostics:\n")
		for _, diagnostic := range inventory.Diagnostics {
			fmt.Fprintf(&out, "  [%s] %s\n", diagnostic.Category, skills.TerminalText(diagnostic.Message))
			for _, path := range diagnostic.Paths {
				fmt.Fprintf(&out, "    %s\n", skills.TerminalText(path))
			}
		}
	}
	return out.String()
}
