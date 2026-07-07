package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newVersionCmd(opts *Opts) *cobra.Command {
	var (
		remote  bool
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show CLI and hub version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			cliVer := version

			if asJSON {
				result := map[string]string{"cli_version": cliVer}
				if remote {
					c := newAdminClient(opts)
					hubVer, err := fetchVersion(c)
					if err != nil {
						result["hub_version_error"] = err.Error()
					} else {
						result["hub_version"] = hubVer
					}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}

			fmt.Fprintf(out, "oah version: %s\n", bold(cliVer))
			if remote {
				c := newAdminClient(opts)
				hubVer, err := fetchVersion(c)
				if err != nil {
					fmt.Fprintf(out, "hub version: %s\n", red("unavailable ("+err.Error()+")"))
				} else {
					fmt.Fprintf(out, "hub version: %s\n", bold(hubVer))
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&remote, "remote", false, "also query the running hub's version")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func fetchVersion(c *adminClient) (string, error) {
	resp, err := c.do("GET", "/admin/version", nil)
	if err != nil {
		return "", fmt.Errorf("cannot reach hub at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", httpErr(resp)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// --- prompt helper additions ---

// readIntDefault prompts for an integer with a default value.
func readIntDefault(prompt string, def int) int {
	s := readLineDefault(prompt, fmt.Sprintf("%d", def))
	n := def
	if s != "" {
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return def
		}
	}
	return n
}

// readRequiredLine keeps prompting until non-empty input is given.
func readRequiredLine(prompt string) string {
	for {
		s := readLine(prompt)
		if s != "" {
			return s
		}
		fmt.Println("  (required — please enter a value)")
	}
}

// readMultiline collects lines until the user enters endMarker on its own line.
func readMultiline(prompt string, endMarker string) string {
	fmt.Printf("%s (end with '%s' on its own line):\n", prompt, endMarker)
	var lines []string
	for {
		line, _ := stdinReader.ReadString('\n')
		line = strings.TrimRight(line, "\n\r")
		if line == endMarker {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
