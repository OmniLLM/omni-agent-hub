package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newMessageCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "message",
		Aliases: []string{"msg"},
		Short:   "Send messages to upstream agents",
	}
	cmd.AddCommand(newMessageSendCmd(opts))
	return cmd
}

func newMessageSendCmd(opts *Opts) *cobra.Command {
	var (
		upstreamFlag string
		text         string
		contextID    string
		skillID      string
		asJSON       bool
	)

	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a message to an upstream agent",
		Long: `Send a message to a specific upstream agent.

When called without flags, enters interactive mode: pick an upstream,
optionally pick a skill, then type a message.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)

			// Interactive mode: fill in missing values.
			if upstreamFlag == "" || text == "" {
				fmt.Println("── Send message (interactive) ──")

				if upstreamFlag == "" {
					u, err := selectUpstream(c)
					if err != nil {
						return err
					}
					if u == nil {
						return nil
					}
					upstreamFlag = u.ID
					fmt.Printf("  Selected: %s (%s)\n", u.Name, u.BaseURL)

					// Offer skill selection if upstream has skills.
					if u.Skills > 0 {
						skills, err := fetchUpstreamSkills(c, u.Name)
						if err == nil && len(skills) > 0 {
							opts := make([]string, len(skills)+1)
							opts[0] = "(no specific skill)"
							for i, sk := range skills {
								opts[i+1] = fmt.Sprintf("%-20s %s", sk.ID, sk.Name)
							}
							idx := readChoice("\nTarget skill:", opts)
							if idx > 0 {
								skillID = skills[idx-1].ID
							}
						}
					}
				}

				if text == "" {
					text = readLine("\nMessage: ")
					if text == "" {
						fmt.Println("No message provided.")
						return nil
					}
				}

				if contextID == "" {
					contextID = readLineDefault("Context ID (for multi-turn)", "")
				}

				fmt.Println()
				fmt.Printf("  Upstream : %s\n", upstreamFlag)
				if skillID != "" {
					fmt.Printf("  Skill    : %s\n", skillID)
				}
				if contextID != "" {
					fmt.Printf("  Context  : %s\n", contextID)
				}
				fmt.Printf("  Message  : %s\n", text)
				fmt.Println()

				if !confirm("Send this message?", true) {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			payload := map[string]any{
				"upstream_id": upstreamFlag,
				"message":     text,
			}
			if contextID != "" {
				payload["context_id"] = contextID
			}
			if skillID != "" {
				payload["skill_id"] = skillID
			}

			body, _ := json.Marshal(payload)
			resp, err := c.do("POST", "/admin/messages", body)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var result map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}

			fmt.Fprintln(out)
			fmt.Fprintf(out, "  Hub Task ID : %s\n", result["hub_task_id"])
			fmt.Fprintf(out, "  Context ID  : %s\n", result["context_id"])
			fmt.Fprintf(out, "  Upstream    : %s\n", result["upstream_id"])

			// Extract response text from the task result if available.
			if r, ok := result["result"].(map[string]any); ok {
				if status, ok := r["status"].(map[string]any); ok {
					fmt.Fprintf(out, "  State       : %s\n", colorStatus(fmt.Sprint(status["state"])))
					if msg, ok := status["message"].(map[string]any); ok {
						if parts, ok := msg["parts"].([]any); ok {
							for _, p := range parts {
								if pm, ok := p.(map[string]any); ok {
									if t, ok := pm["text"].(string); ok && t != "" {
										fmt.Fprintf(out, "\n%s\n",
											green("  Response: ")+t)
									}
								}
							}
						}
					}
				}
			}
			fmt.Fprintln(out)
			return nil
		},
	}

	cmd.Flags().StringVar(&upstreamFlag, "upstream", "", "upstream ID or name")
	cmd.Flags().StringVar(&text, "text", "", "message text")
	cmd.Flags().StringVar(&contextID, "context", "", "context ID for multi-turn")
	cmd.Flags().StringVar(&skillID, "skill", "", "target skill ID")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// skillEntry holds a skill from the admin skills API.
type skillEntry struct {
	ID   string
	Name string
}

// fetchUpstreamSkills returns skills for a specific upstream.
func fetchUpstreamSkills(c *adminClient, upstreamName string) ([]skillEntry, error) {
	resp, err := c.do("GET", "/admin/skills", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var skills []struct {
		SkillID      string `json:"skill_id"`
		LocalSkillID string `json:"local_skill_id"`
		Name         string `json:"name"`
		Upstream     string `json:"upstream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&skills); err != nil {
		return nil, err
	}
	var out []skillEntry
	for _, s := range skills {
		if strings.EqualFold(s.Upstream, upstreamName) {
			out = append(out, skillEntry{ID: s.LocalSkillID, Name: s.Name})
		}
	}
	return out, nil
}

// selectSkill presents a numbered list of skills and returns the chosen one.
func selectSkill(skills []skillEntry) *skillEntry {
	if len(skills) == 0 {
		return nil
	}
	opts := make([]string, len(skills))
	for i, s := range skills {
		opts[i] = s.ID + " — " + s.Name
	}
	idx := readChoice("Select skill:", opts)
	if idx < 0 {
		return nil
	}
	return &skills[idx]
}

// readIntInput reads a line and parses it as an integer.
// Returns def on empty/invalid input.
func readIntInput(prompt string, def int) int {
	s := readLineDefault(prompt, fmt.Sprintf("%d", def))
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
