//go:build amd64 || arm64
// +build amd64 arm64

package machine

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/validate"
	"github.com/containers/podman/v4/libpod/events"
	"github.com/containers/podman/v4/pkg/machine"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	// Pull in configured json library
	json = registry.JSONLibrary()

	sockPaths     []string   // Paths to unix domain sockets for publishing
	openEventSock sync.Once  // Singleton support for opening sockets as needed
	sockets       []net.Conn // Opened sockets, if any

	// Command: podman _machine_
	machineCmd = &cobra.Command{
		Use:                "machine",
		Short:              "Manage a virtual machine",
		Long:               "Manage a virtual machine. Virtual machines are used to run Podman.",
		PersistentPreRunE:  initMachineEvents,
		PersistentPostRunE: closeMachineEvents,
		RunE:               validate.SubCommandExists,
	}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: machineCmd,
	})
}

// autocompleteMachineSSH - Autocomplete machine ssh command.
func autocompleteMachineSSH(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return getMachines(toComplete)
	}
	return nil, cobra.ShellCompDirectiveDefault
}

// autocompleteMachine - Autocomplete machines.
func autocompleteMachine(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return getMachines(toComplete)
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func getMachines(toComplete string) ([]string, cobra.ShellCompDirective) {
	suggestions := []string{}
	provider := getSystemDefaultProvider()
	machines, err := provider.List(machine.ListOptions{})
	if err != nil {
		cobra.CompErrorln(err.Error())
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	for _, m := range machines {
		if strings.HasPrefix(m.Name, toComplete) {
			suggestions = append(suggestions, m.Name)
		}
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

func initMachineEvents(cmd *cobra.Command, _ []string) error {
	logrus.Debugf("Called machine %s.PersistentPreRunE(%s)", cmd.Name(), strings.Join(os.Args, " "))

	sockPaths, err := resolveEventSock()
	if err != nil {
		return err
	}

	// No sockets found, so no need to publish events...
	if len(sockPaths) == 0 {
		return nil
	}

	for _, path := range sockPaths {
		conn, err := (&net.Dialer{}).DialContext(registry.Context(), "unix", path)
		if err != nil {
			logrus.Warnf("Failed to open event socket %q: %v", path, err)
			continue
		}
		logrus.Debugf("Machine event socket %q found", path)
		sockets = append(sockets, conn)
	}
	return nil
}

func resolveEventSock() ([]string, error) {
	// Used mostly for testing
	if sock, found := os.LookupEnv("PODMAN_MACHINE_EVENTS_SOCK"); found {
		return []string{sock}, nil
	}

	xdg, err := util.GetRuntimeDir()
	if err != nil {
		logrus.Warnf("Failed to get runtime dir, machine events will not be published: %s", err)
		return nil, nil
	}

	re := regexp.MustCompile(`machine_events.*\.sock`)
	sockPaths := make([]string, 0)
	fn := func(path string, info os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case info.IsDir():
			return nil
		case info.Type() != os.ModeSocket:
			return nil
		case !re.MatchString(info.Name()):
			return nil
		}

		logrus.Debugf("Machine events will be published on: %q", path)
		sockPaths = append(sockPaths, path)
		return nil
	}

	if err := filepath.WalkDir(filepath.Join(xdg, "podman"), fn); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return sockPaths, nil
}

func newMachineEvent(status events.Status, event events.Event) {
	openEventSock.Do(func() {
		// No sockets where found, so no need to publish events...
		if len(sockPaths) == 0 {
			return
		}

		for _, path := range sockPaths {
			conn, err := (&net.Dialer{}).DialContext(registry.Context(), "unix", path)
			if err != nil {
				logrus.Warnf("Failed to open event socket %q: %v", path, err)
				continue
			}
			logrus.Debugf("Machine event socket %q found", path)
			sockets = append(sockets, conn)
		}
	})

	event.Status = status
	event.Time = time.Now()
	event.Type = events.Machine

	payload, err := json.Marshal(event)
	if err != nil {
		logrus.Errorf("Unable to format machine event: %q", err)
		return
	}

	for _, sock := range sockets {
		if _, err := sock.Write(payload); err != nil {
			logrus.Errorf("Unable to write machine event: %q", err)
		}
	}
}

func closeMachineEvents(cmd *cobra.Command, _ []string) error {
	logrus.Debugf("Called machine %s.PersistentPostRunE(%s)", cmd.Name(), strings.Join(os.Args, " "))
	for _, sock := range sockets {
		_ = sock.Close()
	}
	return nil
}
