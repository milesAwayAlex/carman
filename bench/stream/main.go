package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// worlds are the worlds there are, by name: the pilot's is its own, so
// that the stream's tasks are seen by no arm before they are run.
var worlds = map[string]Rules{
	"pilot":  pilotRules,
	"stream": streamRules,
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stream:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("say: world, admin task ID DAY, admin state, or score TASKS STATE")
	}
	local := "http://127.0.0.1" + or(os.Getenv("WORLD_LISTEN"), ":8080")
	switch args[0] {
	case "world":
		rules, ok := worlds[or(os.Getenv("WORLD_NAME"), "pilot")]
		if !ok {
			return fmt.Errorf("no world called %q", os.Getenv("WORLD_NAME"))
		}
		return serve(newWorld(rules), os.Getenv("WORLD_ADMIN"), or(os.Getenv("WORLD_LISTEN"), ":8080"))
	case "admin":
		if len(args) == 4 && args[1] == "task" {
			day, err := strconv.Atoi(args[3])
			if err != nil {
				return fmt.Errorf("a day is a number")
			}
			body, _ := json.Marshal(map[string]any{"task": args[2], "day": day})
			_, err = admin(http.MethodPost, local+"/admin/task", body)
			return err
		}
		if len(args) == 2 && args[1] == "state" {
			b, err := admin(http.MethodGet, local+"/admin/state", nil)
			if err == nil {
				os.Stdout.Write(b)
			}
			return err
		}
		return fmt.Errorf("say: admin task ID DAY, or admin state")
	case "score":
		if len(args) != 3 {
			return fmt.Errorf("say: score TASKS STATE")
		}
		var tasks []Task
		var state State
		if err := readJSON(args[1], &tasks); err != nil {
			return err
		}
		if err := readJSON(args[2], &state); err != nil {
			return err
		}
		results, err := Score(tasks, state)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(results)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func admin(method, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin", os.Getenv("WORLD_ADMIN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the world answered %s", resp.Status)
	}
	return b, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
