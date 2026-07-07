package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSkillsCmd(opts *Opts) *cobra.Command {
	var (
		upstream string
		match    string
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "skills",
		Short: "List all skills across upstream agents",
		Long:  "Show a table of all skills advertised by healthy upstream agents.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)
			resp, err := c.do("GET", "/admin/skills", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var skills []struct {
				SkillID      string `json:"skill_id"`
				LocalSkillID string `json:"local_skill_id"`
				Name         string `json:"name"`
				Description  string `json:"description"`
				Upstream     string `json:"upstream"`
				UpstreamID   string `json:"upstream_id"`
				Status       string `json:"status"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&skills); err != nil {
				return err
			}

			// Client-side filtering.
			var filtered []struct {
				SkillID      string `json:"skill_id"`
				LocalSkillID string `json:"local_skill_id"`
				Name         string `json:"name"`
				Description  string `json:"description"`
				Upstream     string `json:"upstream"`
				UpstreamID   string `json:"upstream_id"`
				Status       string `json:"status"`
			}
			for _, s := range skills {
				if upstream != "" && !strings.EqualFold(s.Upstream, upstream) {
					continue
				}
				if match != "" && !strings.Contains(strings.ToLower(s.SkillID+s.Name+s.Description), strings.ToLower(match)) {
					continue
				}
				filtered = append(filtered, s)
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(filtered)
			}

			if len(filtered) == 0 {
				fmt.Fprintln(out, "No skills found.")
				return nil
			}

			fmt.Fprintf(out, "\n%-30s %-20s %-12s %s\n", "SKILL_ID", "NAME", "UPSTREAM", "DESCRIPTION")
			fmt.Fprintln(out, separator(90))
			for _, s := range filtered {
				desc := s.Description
				if len(desc) > 40 {
					desc = desc[:40] + "…"
				}
				fmt.Fprintf(out, "%-30s %-20s %-12s %s\n",
					s.SkillID, s.Name, colorStatus(s.Status)+" "+s.Upstream, desc)
			}
			fmt.Fprintf(out, "\n%d skill(s)\n\n", len(filtered))
			return nil
		},
	}

	cmd.Flags().StringVar(&upstream, "upstream", "", "filter by upstream name")
	cmd.Flags().StringVar(&match, "match", "", "substring match on skill id/name/description")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}
