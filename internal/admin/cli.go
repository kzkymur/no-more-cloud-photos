package admin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kzkymur/no-more-cloud-photos/internal/config"
	"github.com/kzkymur/no-more-cloud-photos/internal/database"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || (args[0] != "config" && args[0] != "profile") {
		fmt.Fprintln(stderr, "usage: nmcp-admin config (show|set) | profile (create|activate|retire)")
		return 2
	}
	command := args[0]+" "+args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write one JSON value")
	var name, value, file, id string
	switch command {
	case "config show":
	case "config set":
		flags.StringVar(&name, "name", "", "typed setting name")
		flags.StringVar(&value, "value", "", "non-secret setting value")
	case "profile create":
		flags.StringVar(&file, "file", "", "profile JSON file")
	case "profile activate", "profile retire":
		flags.StringVar(&id, "id", "", "profile UUIDv4")
	default:
		fmt.Fprintln(stderr, "usage: unknown administrative command")
		return 2
	}
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	var definitionData []byte
	if command == "config set" {
		if _, err := ConfigValue(name, value); err != nil {
			return finish(stdout, stderr, *jsonOutput, nil, err)
		}
	}
	if command == "profile create" {
		if file == "" {
			return finish(stdout, stderr, *jsonOutput, nil, ErrValidation)
		}
		input, err := os.Open(file)
		if err != nil {
			return finish(stdout, stderr, *jsonOutput, nil, ErrValidation)
		}
		definitionData, err = io.ReadAll(io.LimitReader(input, (2<<20)+1))
		_ = input.Close()
		if err != nil {
			return finish(stdout, stderr, *jsonOutput, nil, ErrValidation)
		}
		if _, err := DecodeProfile(definitionData); err != nil {
			return finish(stdout, stderr, *jsonOutput, nil, err)
		}
	}
	if (command == "profile activate" || command == "profile retire") && !uuidPattern.MatchString(id) {
		return finish(stdout, stderr, *jsonOutput, nil, ErrValidation)
	}
	cfg, err := config.LoadAdmin()
	if err != nil {
		return finish(stdout, stderr, *jsonOutput, nil, err)
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return finish(stdout, stderr, *jsonOutput, nil, err)
	}
	defer pool.Close()
	migrator, err := database.NewMigrator(pool)
	if err != nil {
		return finish(stdout, stderr, *jsonOutput, nil, err)
	}
	status, err := migrator.Status(ctx)
	if err != nil || !status.Ready() {
		if err == nil {
			err = ErrConflict
		}
		return finish(stdout, stderr, *jsonOutput, nil, err)
	}
	service := New(pool)
	var result any
	switch command {
	case "config show":
		result, err = service.ConfigShow(ctx)
	case "config set":
		result, err = service.ConfigSet(ctx, name, value)
	case "profile create":
		definition, decodeErr := DecodeProfile(definitionData)
		err = decodeErr
		if err == nil {
			err = service.ProfileCreate(ctx, definition)
			result = map[string]any{"id": definition.ID, "status": "draft"}
		}
	case "profile activate", "profile retire":
		err = service.ProfileTransition(ctx, id, command == "profile activate")
		result = map[string]any{"id": id, "command": command}
	}
	return finish(stdout, stderr, *jsonOutput, result, err)
}

func finish(stdout, stderr io.Writer, jsonOutput bool, result any, err error) int {
	code := 0
	message := "success"
	if err != nil {
		switch {
		case errors.Is(err, ErrValidation):
			code, message = 2, ErrValidation.Error()
		case errors.Is(err, ErrConfirmation):
			code, message = 3, ErrConfirmation.Error()
		case errors.Is(err, ErrConflict):
			code, message = 4, ErrConflict.Error()
		case errors.Is(err, ErrPartial):
			code, message = 5, ErrPartial.Error()
		default:
			code, message = 1, "administrative operation failed"
		}
		// Intentionally never stringify external errors: pgx errors can carry
		// connection credentials, SQL values, paths, and profile documents.
		_ = json.NewEncoder(stderr).Encode(map[string]any{"level": "ERROR", "message": message, "exit_code": code})
	}
	if jsonOutput {
		if result == nil || err != nil {
			result = map[string]any{"result": result, "exit_code": code, "message": message}
		}
		if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
			return 1
		}
	} else if err == nil {
		if _, writeErr := fmt.Fprintln(stdout, result); writeErr != nil {
			return 1
		}
	}
	return code
}
