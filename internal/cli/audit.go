package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newAuditCmd(opts *Opts) *cobra.Command {
	var (
		upstream  string
		taskID    string
		traceID   string
		event     string
		limit     int
		offset    int
		detail    bool
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show recent dispatch audit log entries",
		Long:  "Display the audit log of recent dispatch events (sends, responses, errors, breaker state changes).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)

			path := fmt.Sprintf("/admin/audit?limit=%d&offset=%d", limit, offset)
			if upstream != "" {
				path += "&upstream_id=" + upstream
			}
			if taskID != "" {
				path += "&hub_task_id=" + taskID
			}
			if traceID != "" {
				path += "&trace_id=" + traceID
			}
			if event != "" {
				path += "&event=" + event
			}

			resp, err := c.do("GET", path, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var data struct {
				Items []struct {
					ID         int64  `json:"id"`
					TS         string `json:"ts"`
					TraceID    string `json:"trace_id"`
					HubTaskID  string `json:"hub_task_id"`
					UpstreamID string `json:"upstream_id"`
					Event      string `json:"event"`
					DetailJSON string `json:"detail_json"`
				} `json:"items"`
				Total int `json:"total"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			if len(data.Items) == 0 {
				fmt.Fprintln(out, "No audit entries found.")
				return nil
			}

			if detail {
				fmt.Fprintf(out, "\n%-13s %-16s %-10s %-10s %-10s %s\n",
					"TIME", "EVENT", "UPSTREAM", "TASK", "TRACE", "DETAIL")
			} else {
				fmt.Fprintf(out, "\n%-13s %-16s %-10s %-10s %-10s\n",
					"TIME", "EVENT", "UPSTREAM", "TASK", "TRACE")
			}
			fmt.Fprintln(out, separator(80))
			for _, e := range data.Items {
				if detail {
					d := e.DetailJSON
					if len(d) > 50 {
						d = d[:50] + "…"
					}
					fmt.Fprintf(out, "%-13s %-16s %-10s %-10s %-10s %s\n",
						formatTimeShort(e.TS), colorEvent(e.Event),
						shortID(e.UpstreamID), shortID(e.HubTaskID),
						shortID(e.TraceID), d)
				} else {
					fmt.Fprintf(out, "%-13s %-16s %-10s %-10s %-10s\n",
						formatTimeShort(e.TS), colorEvent(e.Event),
						shortID(e.UpstreamID), shortID(e.HubTaskID),
						shortID(e.TraceID))
				}
			}
			fmt.Fprintf(out, "\nShowing %d of %d\n\n", len(data.Items), data.Total)
			return nil
		},
	}

	cmd.Flags().StringVar(&upstream, "upstream", "", "filter by upstream ID")
	cmd.Flags().StringVar(&taskID, "task", "", "filter by hub task ID")
	cmd.Flags().StringVar(&traceID, "trace", "", "filter by trace ID")
	cmd.Flags().StringVar(&event, "event", "", "filter by event type (send, resp, error, etc.)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	cmd.Flags().BoolVar(&detail, "detail", false, "include detail JSON column")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func colorEvent(event string) string {
	switch event {
	case "send", "forward":
		return cyan(event)
	case "resp":
		return green(event)
	case "error":
		return red(event)
	case "cancel":
		return yellow(event)
	case "breaker-open", "breaker-blocked":
		return red(event)
	case "breaker-close":
		return green(event)
	case "stream-start":
		return cyan(event)
	case "stream-end":
		return dim(event)
	default:
		return event
	}
}
