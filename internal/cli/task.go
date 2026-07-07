package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

func newTaskCmd(opts *Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "task",
		Aliases: []string{"tasks"},
		Short:   "Manage tasks (list, inspect, cancel)",
	}
	cmd.AddCommand(newTaskListCmd(opts))
	cmd.AddCommand(newTaskInspectCmd(opts))
	cmd.AddCommand(newTaskCancelCmd(opts))
	return cmd
}

func newTaskListCmd(opts *Opts) *cobra.Command {
	var (
		state      string
		upstream   string
		contextID  string
		recent     bool
		limit      int
		offset     int
		asJSON     bool
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks",
		Long:    "List active tasks. Use --recent to include completed/failed/canceled tasks.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newAdminClient(opts)

			path := fmt.Sprintf("/admin/tasks?limit=%d&offset=%d", limit, offset)
			if recent {
				path += "&recent=true"
			}
			if state != "" {
				path += "&state=" + state
			}
			if upstream != "" {
				path += "&upstream_id=" + upstream
			}
			if contextID != "" {
				path += "&context_id=" + contextID
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
					HubTaskID      string `json:"hub_task_id"`
					ContextID      string `json:"context_id"`
					UpstreamID     string `json:"upstream_id"`
					UpstreamTaskID string `json:"upstream_task_id"`
					State          string `json:"state"`
					CreatedAt      string `json:"created_at"`
					UpdatedAt      string `json:"updated_at"`
					HasSnapshot    bool   `json:"has_snapshot"`
				} `json:"items"`
				Total  int `json:"total"`
				Counts struct {
					Submitted     int `json:"submitted"`
					Working       int `json:"working"`
					InputRequired int `json:"input_required"`
					Completed     int `json:"completed"`
					Failed        int `json:"failed"`
					Canceled      int `json:"canceled"`
					Total         int `json:"total"`
				} `json:"counts"`
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

			// Summary.
			ct := data.Counts
			fmt.Fprintf(out, "\nTasks:  %s:%d  %s:%d  %s:%d  %s:%d  %s:%d  %s:%d  (total: %d)\n\n",
				cyan("submitted"), ct.Submitted,
				cyan("working"), ct.Working,
				yellow("input-req"), ct.InputRequired,
				green("completed"), ct.Completed,
				red("failed"), ct.Failed,
				dim("canceled"), ct.Canceled,
				ct.Total,
			)

			if len(data.Items) == 0 {
				fmt.Fprintln(out, "No tasks found.")
				return nil
			}

			fmt.Fprintf(out, "%-10s %-14s %-10s %-10s %-13s %s\n",
				"TASK_ID", "STATE", "UPSTREAM", "CONTEXT", "UPDATED", "UP_TASK")
			fmt.Fprintln(out, separator(80))
			for _, t := range data.Items {
				fmt.Fprintf(out, "%-10s %-14s %-10s %-10s %-13s %s\n",
					shortID(t.HubTaskID),
					colorStatus(t.State),
					shortID(t.UpstreamID),
					shortID(t.ContextID),
					formatTimeShort(t.UpdatedAt),
					shortID(t.UpstreamTaskID),
				)
			}
			fmt.Fprintf(out, "\nShowing %d of %d\n\n", len(data.Items), data.Total)
			return nil
		},
	}

	cmd.Flags().StringVar(&state, "state", "", "filter by state(s), comma-separated")
	cmd.Flags().StringVar(&upstream, "upstream", "", "filter by upstream ID")
	cmd.Flags().StringVar(&contextID, "context", "", "filter by context ID")
	cmd.Flags().BoolVar(&recent, "recent", false, "include terminal (completed/failed/canceled) tasks")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to return")
	cmd.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func newTaskInspectCmd(opts *Opts) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "inspect [task-id]",
		Short: "Show detailed task information",
		Long: `Show detailed information about a task.

If no task-id is provided, interactively select from active tasks.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			taskID := ""
			if len(args) > 0 {
				taskID = args[0]
			} else {
				// Interactive: pick from active tasks.
				entry, err := selectTask(c, true)
				if err != nil {
					return err
				}
				if entry == nil {
					return nil
				}
				taskID = entry.HubTaskID
			}

			resolvedID, err := resolveTaskID(c, taskID)
			if err != nil {
				return err
			}
			taskID = resolvedID

			resp, err := c.do("GET", "/admin/tasks/"+taskID, nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			var data map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			fmt.Fprintln(out)
			fmt.Fprintf(out, "  Hub Task ID     : %s\n", data["hub_task_id"])
			fmt.Fprintf(out, "  Context ID      : %s\n", data["context_id"])
			fmt.Fprintf(out, "  Upstream ID     : %s\n", data["upstream_id"])
			fmt.Fprintf(out, "  Upstream Task ID: %s\n", data["upstream_task_id"])
			fmt.Fprintf(out, "  State           : %s\n", colorStatus(fmt.Sprint(data["state"])))
			fmt.Fprintf(out, "  Created         : %s\n", data["created_at"])
			fmt.Fprintf(out, "  Updated         : %s\n", data["updated_at"])
			if task, ok := data["task"]; ok && task != nil {
				fmt.Fprintln(out, "\n  Task snapshot:")
				b, _ := json.MarshalIndent(task, "    ", "  ")
				fmt.Fprintf(out, "    %s\n", string(b))
			}
			fmt.Fprintln(out)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func newTaskCancelCmd(opts *Opts) *cobra.Command {
	var (
		yes    bool
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "cancel [task-id]",
		Short: "Cancel an active task",
		Long: `Cancel an active task by forwarding a cancel request to the upstream.

If no task-id is provided, interactively select from active tasks.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newAdminClient(opts)

			taskID := ""
			displayID := ""
			if len(args) > 0 {
				taskID = args[0]
				displayID = shortID(taskID)
			} else {
				entry, err := selectTask(c, false)
				if err != nil {
					return err
				}
				if entry == nil {
					return nil
				}
				taskID = entry.HubTaskID
				displayID = shortID(taskID) + " (" + entry.State + ")"
			}

			if !yes && !confirm(fmt.Sprintf("Cancel task %s?", displayID), false) {
				fmt.Println("Cancelled.")
				return nil
			}

			resolvedID, err := resolveTaskID(c, taskID)
			if err != nil {
				return err
			}
			taskID = resolvedID

			resp, err := c.do("POST", "/admin/tasks/"+taskID+"/cancel", nil)
			if err != nil {
				return fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
			}
			defer resp.Body.Close()

			out := cmd.OutOrStdout()
			if resp.StatusCode != 200 {
				return httpErr(resp)
			}

			if asJSON {
				var data map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
					return err
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(data)
			}

			fmt.Fprintf(out, "✓ Cancel requested for task %s\n", shortID(taskID))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// --- Task selection helper ---------------------------------------------------

type taskEntry struct {
	HubTaskID  string `json:"hub_task_id"`
	UpstreamID string `json:"upstream_id"`
	State      string `json:"state"`
	ContextID  string `json:"context_id"`
	UpdatedAt  string `json:"updated_at"`
}

func selectTask(c *adminClient, includeRecent bool) (*taskEntry, error) {
	path := "/admin/tasks?limit=20"
	if includeRecent {
		path += "&recent=true"
	}
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var data struct {
		Items []taskEntry `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Items) == 0 {
		fmt.Println("No tasks found.")
		return nil, nil
	}

	opts := make([]string, len(data.Items))
	for i, t := range data.Items {
		opts[i] = fmt.Sprintf("%-10s %-14s %-10s %s",
			shortID(t.HubTaskID), t.State, shortID(t.UpstreamID), formatTimeShort(t.UpdatedAt))
	}
	fmt.Println("\n" + strings.Repeat(" ", 6) + fmt.Sprintf("%-10s %-14s %-10s %s", "TASK_ID", "STATE", "UPSTREAM", "UPDATED"))
	idx := readChoice("Select task:", opts)
	if idx < 0 {
		return nil, nil
	}
	return &data.Items[idx], nil
}

func resolveTaskID(c *adminClient, taskID string) (string, error) {
	if taskID == "" {
		return "", nil
	}
	if len(taskID) >= 8 && strings.Contains(taskID, "-") {
		return taskID, nil
	}

	path := "/admin/tasks?limit=20&recent=true"
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return "", fmt.Errorf("cannot reach hub at %s — is it running?\n  %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpErr(resp)
	}

	var data struct {
		Items []taskEntry `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	for _, item := range data.Items {
		if strings.HasPrefix(item.HubTaskID, taskID) {
			return item.HubTaskID, nil
		}
	}

	return taskID, nil
}
