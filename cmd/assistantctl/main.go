package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"work-assistant/internal/apiclient"
	"work-assistant/internal/backup"
	"work-assistant/internal/model"
)

func main() {
	global := flag.NewFlagSet("assistantctl", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	controlURL := global.String("url", envOr("ASSISTANT_URL", "http://127.0.0.1:7337"), "control server URL")
	token := global.String("token", os.Getenv("ASSISTANT_API_TOKEN"), "API bearer token")
	if err := global.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	arguments := global.Args()
	if len(arguments) == 0 {
		usage()
		os.Exit(2)
	}
	client, err := apiclient.New(*controlURL, *token)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch arguments[0] {
	case "role":
		err = roleCommand(ctx, client, arguments[1:])
	case "agent":
		err = agentCommand(ctx, client, arguments[1:])
	case "task":
		err = taskCommand(ctx, client, arguments[1:])
	case "run":
		err = runCommand(ctx, client, arguments[1:])
	case "runtime":
		err = runtimeCommand(ctx, client, arguments[1:])
	case "session":
		err = sessionCommand(ctx, client, arguments[1:])
	case "directive":
		err = directiveCommand(ctx, client, arguments[1:])
	case "event":
		err = eventCommand(ctx, client, arguments[1:])
	case "backup":
		err = backupCommand(ctx, client, arguments[1:])
	case "backup-verify":
		flags := newFlags("backup-verify")
		path := flags.String("from", "", "recovery point directory containing manifest.json")
		if err = flags.Parse(arguments[1:]); err == nil {
			var result backup.Snapshot
			result, err = backup.Verify(ctx, *path)
			if err == nil {
				err = printResult(result, nil)
			}
		}
	case "restore":
		flags := newFlags("restore")
		path := flags.String("from", "", "verified recovery point directory")
		destination := flags.String("to", "", "new, non-existing recovery directory; never live data")
		if err = flags.Parse(arguments[1:]); err == nil {
			var result backup.Snapshot
			result, err = backup.Restore(ctx, *path, *destination)
			if err == nil {
				err = printResult(map[string]any{"snapshot": result, "directory": *destination, "fenced": true, "next_step": "Review pending commands/events, active runs and Agent session files offline before removing RECOVERY_REQUIRED. No service was started."}, nil)
			}
		}
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", arguments[0])
	}
	if err != nil {
		fatal(err)
	}
}

func taskCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("task requires create, split, list, or show")
	}
	switch arguments[0] {
	case "create":
		flags := newFlags("task create")
		title := flags.String("title", "", "task title")
		goal := flags.String("goal", "", "task goal")
		key := flags.String("idempotency-key", "", "source idempotency key")
		needs := requirementFlags(flags)
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		task, err := client.CreateTask(ctx, *title, *goal, *key, needs())
		return printResult(task, err)
	case "split":
		flags := newFlags("task split")
		parent := flags.String("parent", "", "parent task id")
		createdByRun := flags.String("created-by-run", "", "run that requested the split")
		title := flags.String("title", "", "subtask title")
		goal := flags.String("goal", "", "subtask goal")
		key := flags.String("idempotency-key", "", "split idempotency key")
		needs := requirementFlags(flags)
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *parent == "" {
			return errors.New("--parent is required")
		}
		result, err := client.CreateSubtask(ctx, *parent, *createdByRun, *title, *goal, *key, needs())
		return printResult(result, err)
	case "list":
		if len(arguments) != 1 {
			return errors.New("task list takes no arguments")
		}
		tasks, err := client.ListTasks(ctx)
		return printResult(tasks, err)
	case "show":
		flags := newFlags("task show")
		id := flags.String("id", "", "task id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--id is required")
		}
		detail, err := client.GetTask(ctx, *id)
		return printResult(detail, err)
	default:
		return fmt.Errorf("unknown task command %q", arguments[0])
	}
}

func runCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("run requires start, show, direct, or interrupt")
	}
	switch arguments[0] {
	case "start":
		flags := newFlags("run start")
		taskID := flags.String("task", "", "task id")
		sessionID := flags.String("session", "", "existing session id")
		runtimeID := flags.String("runtime", "", "runtime id")
		agentID := flags.String("agent-id", "", "logical agent identity for a new session")
		adapterID := flags.String("adapter", "", "agent adapter id")
		modelID := flags.String("model", "", "model id for a new agent session")
		legacyAdapterID := flags.String("agent", "", "deprecated alias for --adapter")
		workDir := flags.String("workdir", "", "directory under the runtime work root")
		key := flags.String("idempotency-key", "", "run idempotency key")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		command := flags.Args()
		if *taskID == "" {
			return errors.New("--task is required")
		}
		if *adapterID == "" {
			*adapterID = *legacyAdapterID
		}
		run, err := client.CreateRun(ctx, *taskID, *sessionID, *runtimeID, *agentID,
			*adapterID, *modelID, *workDir, *key, command)
		return printResult(run, err)
	case "show":
		flags := newFlags("run show")
		id := flags.String("id", "", "run id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--id is required")
		}
		run, err := client.GetRun(ctx, *id)
		return printResult(run, err)
	case "interrupt":
		flags := newFlags("run interrupt")
		id := flags.String("id", "", "run id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--id is required")
		}
		result, err := client.InterruptRun(ctx, *id)
		return printResult(result, err)
	case "direct":
		flags := newFlags("run direct")
		id := flags.String("id", "", "run id")
		kind := flags.String("kind", "message", "message or interrupt")
		message := flags.String("message", "", "guidance for the running agent")
		key := flags.String("idempotency-key", "", "directive idempotency key")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--id is required")
		}
		directive, err := client.CreateDirective(ctx, *id, *kind, *message, *key)
		return printResult(directive, err)
	default:
		return fmt.Errorf("unknown run command %q", arguments[0])
	}
}

func sessionCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("session requires list or show")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("session list takes no arguments")
		}
		sessions, err := client.ListSessions(ctx)
		return printResult(sessions, err)
	case "show":
		flags := newFlags("session show")
		id := flags.String("id", "", "session id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *id == "" {
			return errors.New("--id is required")
		}
		detail, err := client.GetSession(ctx, *id)
		return printResult(detail, err)
	default:
		return fmt.Errorf("unknown session command %q", arguments[0])
	}
}

func directiveCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "show" {
		return errors.New("directive supports only: directive show --id ID")
	}
	flags := newFlags("directive show")
	id := flags.String("id", "", "directive id")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	directive, err := client.GetDirective(ctx, *id)
	return printResult(directive, err)
}

func runtimeCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) != 1 || arguments[0] != "list" {
		return errors.New("runtime supports only: runtime list")
	}
	runtimes, err := client.ListRuntimes(ctx)
	return printResult(runtimes, err)
}

func eventCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "watch" {
		return errors.New("event supports only: event watch [--after N]")
	}
	flags := newFlags("event watch")
	after := flags.String("after", "0", "last seen global event sequence")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	sequence, err := strconv.ParseInt(*after, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid --after: %w", err)
	}
	return client.WatchEvents(ctx, sequence, os.Stdout)
}

func backupCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	flags := newFlags("backup")
	output := flags.String("output", "", "destination SQLite file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output is required")
	}
	if err := client.Backup(ctx, *output); err != nil {
		return err
	}
	return printResult(map[string]any{"backup": *output}, nil)
}

func printResult(value any, err error) error {
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	return flags
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "assistantctl:", err)
	os.Exit(1)
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, `Usage:
	assistantctl role draft [--description TEXT] [--from ROLE_ID]
	assistantctl role chat --draft DRAFT_ID --message TEXT [--runtime ID] [--model MODEL]
	assistantctl role show --draft DRAFT_ID | --id ROLE_ID
	assistantctl role edit --draft DRAFT_ID --file role.json [--version N]
	assistantctl role publish --draft DRAFT_ID [--version N]
	assistantctl role list [--drafts]
	assistantctl agent create --name NAME --role ROLE_ID --runtime ID [--adapter ADAPTER] [--model MODEL]
	assistantctl agent list
  assistantctl [--url URL] task create --title TITLE --goal GOAL [--idempotency-key KEY]
  assistantctl [--url URL] task split --parent TASK_ID --title TITLE --goal GOAL [--created-by-run RUN_ID]
  assistantctl [--url URL] task list
  assistantctl [--url URL] task show --id TASK_ID
  assistantctl [--url URL] run start --task TASK_ID [--session SESSION_ID] [--runtime RUNTIME_ID] [--adapter ADAPTER] [--model MODEL] [--workdir DIR] [-- COMMAND [ARG...]]
  assistantctl [--url URL] run show --id RUN_ID
  assistantctl [--url URL] run direct --id RUN_ID --message TEXT
  assistantctl [--url URL] run interrupt --id RUN_ID
  assistantctl [--url URL] directive show --id DIRECTIVE_ID
  assistantctl [--url URL] session list
  assistantctl [--url URL] session show --id SESSION_ID
  assistantctl [--url URL] runtime list
  assistantctl [--url URL] event watch [--after GLOBAL_SEQ]
  assistantctl [--url URL] backup --output FILE
  assistantctl backup-verify --from SNAPSHOT_DIR
  assistantctl restore --from SNAPSHOT_DIR --to NEW_DIRECTORY`)
}

func requirementFlags(flags *flag.FlagSet) func() model.TaskRequirements {
	role := flags.String("role", "", "required published role id")
	capabilities := flags.String("requires", "", "required capabilities, comma separated")
	excluded := flags.String("exclude-agents", "", "excluded agent ids, comma separated (e.g. the author of reviewed work)")
	return func() model.TaskRequirements {
		return model.TaskRequirements{RoleID: *role, Capabilities: splitCSV(*capabilities), ExcludedAgentIDs: splitCSV(*excluded)}
	}
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
