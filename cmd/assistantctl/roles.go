package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"work-assistant/internal/apiclient"
	"work-assistant/internal/model"
)

func roleCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("role requires draft, chat, edit, publish, show, or list")
	}
	command := arguments[0]
	flags := newFlags("role " + command)
	switch command {
	case "draft":
		description := flags.String("description", "", "initial role description")
		source := flags.String("from", "", "duplicate a published role")
		key := flags.String("idempotency-key", "", "idempotency key")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		result, err := client.CreateRoleDraft(ctx, *description, *source, *key)
		return printResult(result, err)
	case "list":
		drafts := flags.Bool("drafts", false, "list drafts instead of published roles")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *drafts {
			result, err := client.ListRoleDrafts(ctx)
			return printResult(result, err)
		}
		result, err := client.ListRoles(ctx)
		return printResult(result, err)
	case "show":
		draftID := flags.String("draft", "", "draft id")
		roleID := flags.String("id", "", "published role id")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if (*draftID == "") == (*roleID == "") {
			return errors.New("specify exactly one of --draft and --id")
		}
		if *draftID != "" {
			result, err := client.GetRoleDraft(ctx, *draftID)
			return printResult(result, err)
		}
		result, err := client.GetRole(ctx, *roleID)
		return printResult(result, err)
	case "chat", "edit", "publish":
		draftID := flags.String("draft", "", "draft id")
		version := flags.Int64("version", 0, "expected draft version; defaults to a fresh read")
		var message, runtimeID, adapterID, modelID, key, file string
		if command == "chat" {
			flags.StringVar(&message, "message", "", "role requirements or revision request")
			flags.StringVar(&runtimeID, "runtime", "", "builder runtime")
			flags.StringVar(&adapterID, "adapter", "", "builder adapter (default codex-agent for a new session)")
			flags.StringVar(&modelID, "model", "", "model for a new builder session")
			flags.StringVar(&key, "idempotency-key", "", "idempotency key")
		}
		if command == "edit" {
			flags.StringVar(&file, "file", "", "RoleSpec JSON file")
		}
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if *draftID == "" {
			return errors.New("--draft is required")
		}
		if *version == 0 {
			draft, err := client.GetRoleDraft(ctx, *draftID)
			if err != nil {
				return err
			}
			*version = draft.Version
		}
		switch command {
		case "chat":
			result, err := client.ChatRoleDraft(ctx, *draftID, *version, message, runtimeID, adapterID, modelID, key)
			return printResult(result, err)
		case "publish":
			result, err := client.PublishRoleDraft(ctx, *draftID, *version)
			return printResult(result, err)
		case "edit":
			if file == "" {
				return errors.New("--file is required")
			}
			input, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var spec model.RoleSpec
			if err := json.Unmarshal(input, &spec); err != nil {
				return err
			}
			result, err := client.UpdateRoleDraft(ctx, *draftID, *version, spec)
			return printResult(result, err)
		}
	}
	return fmt.Errorf("unknown role command %q", command)
}

func agentCommand(ctx context.Context, client *apiclient.Client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("agent requires create or list")
	}
	if arguments[0] == "list" {
		if len(arguments) != 1 {
			return errors.New("agent list takes no arguments")
		}
		result, err := client.ListAgents(ctx)
		return printResult(result, err)
	}
	if arguments[0] != "create" {
		return fmt.Errorf("unknown agent command %q", arguments[0])
	}
	flags := newFlags("agent create")
	var agent model.AgentProfile
	flags.StringVar(&agent.Name, "name", "", "unique agent name")
	flags.StringVar(&agent.RoleID, "role", "", "published role id")
	flags.StringVar(&agent.RuntimeID, "runtime", "", "runtime id")
	flags.StringVar(&agent.AdapterID, "adapter", "codex-agent", "agent adapter")
	flags.StringVar(&agent.ModelID, "model", "", "model (runtime default when empty)")
	flags.IntVar(&agent.MaxConcurrent, "max-concurrent", 1, "maximum concurrent task sessions")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	result, err := client.CreateAgent(ctx, agent)
	return printResult(result, err)
}
