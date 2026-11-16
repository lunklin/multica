package attachmentadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const CLIVersion = "0.1.0-candidate"
const Help = `Bounded human Owner attachment administration (candidate; not deployed).
Commands (JSON output, no prompts):
  version [--server] [--output json]
  attachment inspect ID [--output json]
  attachment delete ID --dry-run --scope-file scope.json [--output json]
  attachment delete ID --confirm-plan PLAN_ID --request-id REQUEST_ID [--output json]
  attachment operation get REQUEST_ID [--output json]
Scope JSON: {"workspace_id":"UUID","issue_id":"UUID","attachment_ids":[16 exact UUIDs]}
Dry-run persists only a metadata plan (10 minutes); it does not remove anything.
Timeout/UNKNOWN: read the SAME request ID before resuming confirmation.
Only REMOVED_VERIFIED is a completed removal. Storage acknowledgement does not
prove CDN, old URLs, object versions or backups erased. No wildcard/force mode.
Exit: 0 read/preview/completed, 1 failed/pending/unknown, 2 transport,
3 permission, 4 not found (NOT a receipt), 5 invalid input/drift/version/immutable.
The standalone entry uses MULTICA_SERVER_URL, MULTICA_WORKSPACE_ID, MULTICA_TOKEN;
it does not read profiles. Never pass credentials in arguments or URLs.
`

type CLIConfig struct {
	Server, Workspace, Token string
	Machine                  bool
	Client                   *http.Client
}

// RunCLI is shared by the installed CLI command bindings and the small isolated
// distribution. All authorization and mutation policy remains on the server.
func RunCLI(ctx context.Context, args []string, c CLIConfig, out, errout io.Writer) int {
	emit := func(v any) { _ = json.NewEncoder(out).Encode(v) }
	bad := func(code string, exit int) int { emit(map[string]string{"code": code}); return exit }
	if len(args) == 0 || slicesHelp(args) {
		fmt.Fprint(out, Help)
		return 0
	}
	if args[0] == "version" && len(args) == 1 {
		emit(map[string]string{"cli_version": CLIVersion})
		return 0
	}
	if c.Machine || strings.HasPrefix(c.Token, "mat_") || strings.HasPrefix(c.Token, "mcn_") {
		return bad("human_owner_required", 3)
	}
	parsed, err := url.Parse(c.Server)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" || parsed.Path != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return bad("invalid_server_url", 5)
	}
	if !ValidID(c.Workspace) {
		return bad("invalid_workspace", 5)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	call := func(method, path string, input, output any) int {
		var body io.Reader
		if input != nil {
			b, _ := json.Marshal(input)
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Server+Prefix+path, body)
		if err != nil {
			return bad("invalid_request", 5)
		}
		req.Header.Set("X-Workspace-ID", c.Workspace)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Client-Version", CLIVersion)
		resp, err := copyClient.Do(req)
		if err != nil {
			return bad("transport_result_unknown_read_same_request_id", 2)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var e struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
			// Only stable machine codes cross the CLI boundary; never echo an HTTP
			// server's arbitrary response body, signed URLs or reflected credentials.
			if !codePattern(e.Code) {
				e.Code = "server_request_failed"
			}
			exit := 1
			switch resp.StatusCode {
			case 401, 403:
				exit = 3
			case 404:
				exit = 4
			case 400, 409, 422, 405:
				exit = 5
			}
			return bad(e.Code, exit)
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 131072)).Decode(output) != nil {
			return bad("invalid_server_response", 1)
		}
		return 0
	}
	var v Version
	if exit := call("GET", "version", nil, &v); exit != 0 {
		return exit
	}
	if v.Capability != Capability || v.Build == "" {
		return bad("unsupported_server_capability", 5)
	}
	if args[0] == "version" {
		for _, v := range args[1:] {
			if v != "--server" && v != "--output" && v != "json" {
				return bad("invalid_arguments", 5)
			}
		}
		emit(v)
		return 0
	}
	if len(args) < 3 || args[0] != "attachment" {
		return bad("invalid_command", 5)
	}
	path := ""
	method := "GET"
	var input any
	isConfirm := false
	rest := []string{}
	switch args[1] {
	case "inspect":
		if !ValidID(args[2]) {
			return bad("invalid_id", 5)
		}
		path = "attachments/" + args[2]
		rest = args[3:]
	case "operation":
		if len(args) < 4 || args[2] != "get" || !ValidID(args[3]) {
			return bad("invalid_operation", 5)
		}
		path = "operations/" + args[3]
		rest = args[4:]
	case "delete":
		if !ValidID(args[2]) {
			return bad("invalid_id", 5)
		}
		path = "attachments/" + args[2]
		rest = args[3:]
	default:
		return bad("invalid_command", 5)
	}
	f := flag.NewFlagSet("attachment-admin", flag.ContinueOnError)
	f.SetOutput(errout)
	dry := f.Bool("dry-run", false, "metadata preview only")
	scopeFile := f.String("scope-file", "", "16-ID JSON allowlist")
	plan := f.String("confirm-plan", "", "preview ID")
	rid := f.String("request-id", "", "durable operation UUID")
	output := f.String("output", "json", "json only")
	if f.Parse(rest) != nil || f.NArg() != 0 || *output != "json" {
		return bad("invalid_arguments", 5)
	}
	if args[1] == "delete" {
		method = "POST"
		if *dry && *scopeFile != "" && *plan == "" && *rid == "" {
			file, err := os.Open(*scopeFile)
			if err != nil {
				return bad("scope_file_unreadable", 5)
			}
			defer file.Close()
			var scope Scope
			d := json.NewDecoder(io.LimitReader(file, 65537))
			d.DisallowUnknownFields()
			if d.Decode(&scope) != nil || d.Decode(new(any)) != io.EOF {
				return bad("invalid_scope_file", 5)
			}
			input = scope
			path += "/plans"
		} else if !*dry && *scopeFile == "" && ValidID(*plan) && ValidID(*rid) {
			input = map[string]string{"plan_id": *plan, "request_id": *rid}
			path += "/confirm"
			isConfirm = true
		} else {
			return bad("confirmation_or_dry_run_required", 5)
		}
	} else if *dry || *scopeFile != "" || *plan != "" || *rid != "" {
		return bad("invalid_arguments", 5)
	}
	var result json.RawMessage
	if exit := call(method, path, input, &result); exit != 0 {
		return exit
	}
	if !json.Valid(result) {
		return bad("invalid_server_response", 1)
	}
	_, _ = fmt.Fprintln(out, string(result))
	if isConfirm {
		var op Operation
		if json.Unmarshal(result, &op) != nil {
			return 1
		}
		if op.State == Rejected {
			return 5
		}
		if op.State != Removed {
			return 1
		}
	}
	return 0
}
func slicesHelp(args []string) bool {
	for _, v := range args {
		if v == "--help" || v == "-h" {
			return true
		}
	}
	return false
}
func codePattern(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for _, v := range s {
		if !(v >= 'a' && v <= 'z' || v >= '0' && v <= '9' || v == '_') {
			return false
		}
	}
	return true
}
