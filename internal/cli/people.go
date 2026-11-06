package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// peopleRequest keeps -o json's API shape; text prints a compact key/value
// profile or one table row per item. Secrets are only returned by creates.
func (a *app) peopleRequest(cmd *cobra.Command, method, path string, body any, list string) error {
	if method == "DELETE" {
		if err := a.c.Do(ctxOf(cmd), method, path, body, nil); err != nil {
			return err
		}
		if a.output == "json" {
			return a.json(map[string]any{"revoked": true})
		}
		fmt.Fprintln(a.stdout, "done")
		return nil
	}
	var out map[string]any
	if err := a.c.Do(ctxOf(cmd), method, path, body, &out); err != nil {
		return err
	}
	if a.output == "json" {
		if list != "" {
			return a.json(out[list])
		}
		return a.json(out)
	}
	if list != "" {
		items, _ := out[list].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			if list == "results" {
				if v, ok := m["invitation"].(map[string]any); ok {
					fmt.Fprintf(a.stdout, "%v  %v  %v\n", m["email"], v["url"], v["expiresAt"])
				} else {
					fmt.Fprintf(a.stdout, "%v  %v\n", m["email"], m["result"])
				}
			} else {
				fmt.Fprintln(a.stdout, compactPerson(m))
			}
		}
		return nil
	}
	for _, k := range []string{"id", "email", "name", "tenant", "role", "keyId", "apiKey", "prefix", "url", "expiresAt", "operator", "userId", "serviceAccount", "memberships"} {
		if v, ok := out[k]; ok {
			fmt.Fprintf(a.stdout, "%s: %v\n", k, v)
		}
	}
	// A tenant created with its first owner carries that Owner invitation:
	// its link is what the operator sends, so text shows it as create does.
	if v, ok := out["invitation"].(map[string]any); ok {
		fmt.Fprintf(a.stdout, "invitation: %v  %v  %v\n", v["email"], v["url"], v["expiresAt"])
	}
	return nil
}
func compactPerson(m map[string]any) string {
	var out []string
	for _, k := range []string{"id", "email", "name", "role", "status", "prefix", "expiresAt", "activeKeys"} {
		if v, ok := m[k]; ok {
			out = append(out, fmt.Sprint(v))
		}
	}
	return strings.Join(out, "  ")
}
func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: "Your person, tenant, role and key", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error { return a.peopleRequest(c, "GET", "/v1/whoami", nil, "") }}
}
func expiryTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	var d time.Duration
	var err error
	if strings.HasSuffix(s, "d") {
		n, e := strconv.Atoi(strings.TrimSuffix(s, "d"))
		err = e
		d = time.Duration(n) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("expires: want a positive duration, e.g. 90d")
	}
	t := time.Now().Add(d).UTC()
	return &t, nil
}
func (a *app) keysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Your personal API keys in this tenant"}
	var name, expires string
	var ro bool
	create := &cobra.Command{Use: "create --name N", Short: "Make a personal key (shown once)", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		exp, err := expiryTime(expires)
		if err != nil {
			return err
		}
		return a.peopleRequest(c, "POST", "/v1/me/keys", map[string]any{"name": name, "readOnly": ro, "expiresAt": exp}, "")
	}}
	create.Flags().StringVar(&name, "name", "", "key name")
	create.MarkFlagRequired("name")
	create.Flags().BoolVar(&ro, "read-only", false, "read only, whatever your role")
	create.Flags().StringVar(&expires, "expires", "", "expires after a duration, e.g. 90d")
	cmd.AddCommand(create, &cobra.Command{Use: "ls", Short: "List active personal keys", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error { return a.peopleRequest(c, "GET", "/v1/me/keys", nil, "keys") }}, &cobra.Command{Use: "revoke <id|prefix>", Short: "Revoke a key", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "DELETE", "/v1/me/keys/"+url.PathEscape(args[0]), nil, "")
	}}, &cobra.Command{Use: "rename <id|prefix> <name>", Short: "Rename a key", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "PATCH", "/v1/me/keys/"+url.PathEscape(args[0]), map[string]any{"name": args[1]}, "")
	}})
	return cmd
}
func (a *app) membersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "members", Short: "Tenant members and their roles"}
	var role string
	add := &cobra.Command{Use: "add <email> --role R", Short: "Add a member directly (operators only)", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "POST", "/v1/members", map[string]any{"email": args[0], "role": role}, "")
	}}
	add.Flags().StringVar(&role, "role", "member", "viewer, member, admin or owner")
	cmd.AddCommand(add, &cobra.Command{Use: "ls", Short: "List members", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return a.peopleRequest(c, "GET", "/v1/members", nil, "members")
	}}, &cobra.Command{Use: "set-role <email> <role>", Short: "Change a member's role", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "PATCH", "/v1/members/"+url.PathEscape(args[0]), map[string]any{"role": args[1]}, "")
	}}, &cobra.Command{Use: "rm <email>", Short: "Remove a member and revoke their keys", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "DELETE", "/v1/members/"+url.PathEscape(args[0]), nil, "")
	}})
	return cmd
}
func (a *app) invitesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "invites", Short: "Email-bound invitations (Cloudflare Access mode)"}
	var role, status string
	create := &cobra.Command{Use: "create <email>... --role R", Short: "Make links to send to the people you invite", Args: cobra.MinimumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		items := []map[string]string{}
		for _, e := range args {
			items = append(items, map[string]string{"email": e})
		}
		return a.peopleRequest(c, "POST", "/v1/invitations", map[string]any{"invitations": items, "role": role}, "results")
	}}
	create.Flags().StringVar(&role, "role", "member", "viewer, member, admin or owner")
	ls := &cobra.Command{Use: "ls", Short: "List invitations", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return a.peopleRequest(c, "GET", "/v1/invitations?status="+url.QueryEscape(status), nil, "invitations")
	}}
	ls.Flags().StringVar(&status, "status", "all", "pending, expired, accepted, revoked or all")
	cmd.AddCommand(create, ls)
	for _, action := range []string{"renew", "revoke"} {
		cmd.AddCommand(&cobra.Command{Use: action + " <email|id>", Short: action + " an invitation", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
			return a.peopleRequest(c, "POST", "/v1/invitations/"+url.PathEscape(args[0])+"/"+action, nil, "")
		}})
	}
	return cmd
}
