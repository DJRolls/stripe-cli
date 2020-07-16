package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/playback"
	"github.com/stripe/stripe-cli/pkg/validators"
)

const defaultPort = 13111
const defaultWebhookPort = 13112
const endpointsDocString = `
--- Controlling the server ---
You can configure the running server instance via HTTP GET endpoints (prefixed with "/playback/").

--- List of server control endpoints ---
GET /playback/mode/[mode]
Sets the server mode to one of ["auto", "record", "replay"]

GET /playback/cassette/setroot?dir=[path_to_directory]
Set the root directory for reading/writing cassettes. All cassette paths are relative to this directory.

GET /playback/cassette/load?filepath=[filepath]
Load the cassette file at the given filepath, relative to the cassette root directory.

GET /playback/cassette/eject
Eject (unload) the current cassette and do any teardown. In record mode, this writes the recorded interactions to the cassette file.
`

type playbackCmd struct {
	cmd *cobra.Command

	mode string

	apiBaseURL     string
	filepath       string
	cassetteDir    string
	address        string
	webhookAddress string
}

func newPlaybackCmd() *playbackCmd {
	pc := &playbackCmd{}

	pc.cmd = &cobra.Command{
		Hidden: true,
		Use:    "playback",
		Args:   validators.NoArgs,
		Short:  "Start a `playback` server",
		Long: `
--- Overview ---
The playback command starts a local proxy server that intercepts outgoing requests to the Stripe API.

There are three modes of operation:

"record": Any requests received are forwarded to the api.stripe.com, and the response is returned. All interactions
are written to the loaded 'cassette' file for later playback in replay mode.

"replay": All received requests are terminated at the playback server, and responses are played back[1] from a cassette file. A existing cassette most be loaded.

"auto" (default): The server determines whether to run in "record" or "replay" mode on a per-cassette basis. If the cassette exists, operates in "replay" mode. If not, operates in "record" mode.

Currently, stripe playback only supports serving over HTTP.

[1]: requests are currently replayed sequentially in the same order they were recorded.
` + endpointsDocString,
		Example: `stripe playback
  stripe playback --mode replay
  stripe playback --cassette "my_cassette.yaml"`,
		RunE: pc.runPlaybackCmd,
	}

	pc.cmd.Flags().StringVar(&pc.mode, "mode", "auto", "Auto: record if cassette doesn't exist, replay if exists. Record: always record/re-record. Replay: always replay.")
	pc.cmd.Flags().StringVar(&pc.address, "address", fmt.Sprintf("localhost:%d", defaultPort), "Address to serve on")
	pc.cmd.Flags().StringVar(&pc.webhookAddress, "forward-to", fmt.Sprintf("localhost:%d", defaultWebhookPort), "Address to forward webhooks to")
	pc.cmd.Flags().StringVar(&pc.filepath, "cassette", "default_cassette.yaml", "The cassette file to use")
	pc.cmd.Flags().StringVar(&pc.cassetteDir, "cassette-root-dir", "", "Directory to store all cassettes in. Relative cassette paths are considered relative to this directory.")

	// // Hidden configuration flags, useful for dev/debugging
	pc.cmd.Flags().StringVar(&pc.apiBaseURL, "api-base", "https://api.stripe.com", "Sets the API base URL")

	return pc
}

func (pc *playbackCmd) runPlaybackCmd(cmd *cobra.Command, args []string) error {
	fmt.Println()
	fmt.Println("Seting up playback server...")
	fmt.Println()

	// --- Validate command-line args
	// Check that mode is valid
	if pc.mode != playback.Auto && pc.mode != playback.Record && pc.mode != playback.Replay {
		return fmt.Errorf(
			"\"%v\" is not a valid mode. It must be either \"%v\", \"%v\", or \"%v\"",
			pc.mode, playback.Auto, playback.Record, playback.Replay)
	}

	// Check that cassette root directory is valid
	absoluteCassetteDir, err := filepath.Abs(pc.cassetteDir)
	if err != nil {
		return fmt.Errorf("Error with --cassette-root-dir: %w", err)
	}

	handle, err := os.Stat(absoluteCassetteDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the directory \"%v\" does not exist. Please create it, then re-run the command", absoluteCassetteDir)
		}
		return fmt.Errorf("Unexpected error when checking --cassette-root-dir: %w", err)
	}

	if !handle.Mode().IsDir() {
		return fmt.Errorf("The provided `--cassette-root-dir` option is not a valid directory: %v", absoluteCassetteDir)
	}

	// --- Start up the playback HTTP server
	// TODO: `playback` should handle setup (and teardown) of `stripe listen` as well
	addressString := pc.address
	remoteURL := pc.apiBaseURL

	webhookAddress := "http://" + pc.webhookAddress

	httpWrapper, err := playback.NewRecordReplayServer(remoteURL, webhookAddress, absoluteCassetteDir)
	if err != nil {
		return nil
	}

	server := httpWrapper.InitializeServer(addressString)
	go func() {
		err = server.ListenAndServe()
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}()

	fullAddressString := "http://" + addressString

	client := &http.Client{}

	// --- Use the flag values to configure the server (over HTTP)
	resp, err := client.Get(fullAddressString + "/playback/mode/" + pc.mode)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return errors.New("Non 200 status code received during startup: " + resp.Status)
	}

	resp, err = client.Get(fullAddressString + "/playback/cassette/load?filepath=" + pc.filepath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return errors.New("Non 200 status code received during startup: " + resp.Status)
	}

	// --- Print out post-startup summary on CLI
	fmt.Println()
	fmt.Println("------ Server Running ------")

	switch pc.mode {
	case playback.Record:
		fmt.Printf("In \"record\" mode.\n")
		fmt.Println("Will always record interactions, and write (or overwrite) to the given cassette filepath.")
		fmt.Println()
	case playback.Replay:
		fmt.Printf("In \"replay\" mode.\n")
		fmt.Println("Will always replay from the given cassette. Will error if loaded cassette path doesn't exist.")
		fmt.Println()
	case playback.Auto:
		fmt.Printf("In \"auto\" mode.\n")
		fmt.Println("Can both record or replay, depending on the file passed in. If exists, replays. If not, records.")
		fmt.Println()
	}

	fmt.Printf("Cassettes directory: \"%v\".\n", absoluteCassetteDir)
	fmt.Printf("Using cassette: \"%v\".\n", pc.filepath)
	fmt.Println()

	fmt.Printf("Listening via HTTP on %v\n", addressString)

	fmt.Printf("Forwarding webhooks to %v\n", webhookAddress)
	fmt.Println("-----------------------------")
	fmt.Println()

	select {}
}
