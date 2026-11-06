package cli

import (
	"github.com/spf13/cobra"
	"net/url"
)

func (a *app) saCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sa", Short: "Service accounts"}
	var role, owner, description string
	create := &cobra.Command{Use: "create <name> --role R", Short: "Create a service account (admins only)", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "POST", "/v1/service-accounts", map[string]any{"name": args[0], "role": role, "ownerEmail": owner, "description": description}, "")
	}}
	create.Flags().StringVar(&role, "role", "member", "viewer, member or admin")
	create.Flags().StringVar(&owner, "owner", "", "owner's email (must be a member)")
	create.Flags().StringVar(&description, "description", "", "description")
	cmd.AddCommand(create, &cobra.Command{Use: "ls", Short: "List service accounts", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return a.peopleRequest(c, "GET", "/v1/service-accounts", nil, "serviceAccounts")
	}}, &cobra.Command{Use: "get <sa>", Short: "Get a service account", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "GET", "/v1/service-accounts/"+url.PathEscape(args[0]), nil, "")
	}}, &cobra.Command{Use: "disable <sa>", Short: "Disable a service account", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "PATCH", "/v1/service-accounts/"+url.PathEscape(args[0]), map[string]any{"disabled": true}, "")
	}})
	var setRole, setOwner, setDesc, setName string
	var disabled bool
	set := &cobra.Command{Use: "set <sa>", Short: "Change name, role, owner, description or disabled", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		b := map[string]any{}
		for flag, v := range map[string]any{"name": setName, "role": setRole, "owner": setOwner, "description": setDesc, "disabled": disabled} {
			if c.Flags().Changed(flag) {
				k := flag
				if k == "owner" {
					k = "ownerEmail"
				}
				b[k] = v
			}
		}
		return a.peopleRequest(c, "PATCH", "/v1/service-accounts/"+url.PathEscape(args[0]), b, "")
	}}
	set.Flags().StringVar(&setName, "name", "", "a new name (its key named as the account follows)")
	set.Flags().StringVar(&setRole, "role", "", "viewer, member or admin")
	set.Flags().StringVar(&setOwner, "owner", "", "owner email (empty clears)")
	set.Flags().StringVar(&setDesc, "description", "", "description")
	set.Flags().BoolVar(&disabled, "disabled", false, "disable or enable")
	cmd.AddCommand(set)
	keys := &cobra.Command{Use: "keys", Short: "Rotate service account keys (owner or admins)"}
	var name string
	kc := &cobra.Command{Use: "create <sa> --name N", Short: "Create a key (shown once)", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "POST", "/v1/service-accounts/"+url.PathEscape(args[0])+"/keys", map[string]any{"name": name}, "")
	}}
	kc.Flags().StringVar(&name, "name", "", "key name")
	kc.MarkFlagRequired("name")
	keys.AddCommand(kc, &cobra.Command{Use: "ls <sa>", Short: "List active keys", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "GET", "/v1/service-accounts/"+url.PathEscape(args[0])+"/keys", nil, "keys")
	}}, &cobra.Command{Use: "revoke <sa> <id|prefix>", Short: "Revoke a key", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "DELETE", "/v1/service-accounts/"+url.PathEscape(args[0])+"/keys/"+url.PathEscape(args[1]), nil, "")
	}})
	cmd.AddCommand(keys)
	return cmd
}
func (a *app) tenantCreateCmd() *cobra.Command {
	var owner string
	cmd := &cobra.Command{Use: "create <name>", Short: "Create a tenant and optionally invite its first owner", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.peopleRequest(c, "POST", "/v1/tenants", map[string]any{"name": args[0], "ownerEmail": owner}, "")
	}}
	cmd.Flags().StringVar(&owner, "owner", "", "first owner's email")
	return cmd
}

// tenantSetOwnerCmd: POST /v1/members?tenant=<tenant> {email, role: owner,
// upsert: true}. Operators only: a member becomes owner, anyone else joins
// as one; no invitation.
func (a *app) tenantSetOwnerCmd() *cobra.Command {
	return &cobra.Command{Use: "set-owner <tenant> <email>", Short: "Make someone an owner of a tenant (operators only): a member is promoted, anyone else added", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		a.c.Tenant = args[0]
		return a.peopleRequest(c, "POST", "/v1/members", map[string]any{"email": args[1], "role": "owner", "upsert": true}, "")
	}}
}
