package command

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/lipgloss"
	"github.com/jedib0t/go-pretty/table"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/anchore/clio"
	gobin "github.com/khulnasoft-lab/gobin"
	"github.com/khulnasoft-lab/gobin/cmd/gobin/cli/option"
	"github.com/khulnasoft-lab/gobin/internal/bus"
	"github.com/khulnasoft-lab/gobin/tool"
)

type ListConfig struct {
	Config       string `json:"config" yaml:"config" mapstructure:"config"`
	option.Check `json:"" yaml:",inline" mapstructure:",squash"`
	option.Core  `json:"" yaml:",inline" mapstructure:",squash"`
	option.List  `json:"" yaml:",inline" mapstructure:",squash"`
}

func List(app clio.Application) *cobra.Command {
	cfg := &ListConfig{
		Core: option.DefaultCore(),
	}

	return app.SetupCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured and installed tool status",
		Aliases: []string{
			"ls",
		},
		Args: cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(*cfg)
		},
	}, cfg)
}

type toolStatus struct {
	Name             string `json:"name"`
	WantedVersion    string `json:"wantedVersion"`    // this is the version the user asked for
	ResolvedVersion  string `json:"resolvedVersion"`  // if the user asks for a non-specific version (e.g. "latest") then this is what that would resolve to at this point in time
	Constraint       string `json:"constraint"`       // the version constraint the user asked for and used during version resolution
	IsInstalled      bool   `json:"isInstalled"`      // is the tool installed at the desired version (says nothing about it being valid, only present)
	HashIsValid      bool   `json:"hashIsValid"`      // is the installed tool have the correct xxh64 hash?
	InstalledVersion string `json:"installedVersion"` // the actual version that is installed, which could vary from the user wanted or resolved values
	Error            error  `json:"error,omitempty"`  // if there was an error getting the status for this tool, it will be here
}

func runList(cmdCfg ListConfig) error {
	// get the current store state
	store, err := gobin.NewStore(cmdCfg.Store.Root)
	if err != nil {
		return err
	}

	allStatuses := getAllStatuses(cmdCfg, store)

	// look for items in the store root that cannot be accounted for
	// TODO

	if cmdCfg.List.Updates {
		return presentUpdates(allStatuses)
	}

	return presentList(allStatuses)
}

func getAllStatuses(cmdCfg ListConfig, store *gobin.Store) []toolStatus {
	var (
		failedTools = make(map[string]error)
		allStatus   []toolStatus
	)

	names, toolOpts := selectNamesAndConfigs(cmdCfg.Core, nil)

	storedEntries := store.Entries()

	for _, opt := range toolOpts {
		status, entry, err := getStatus(store, opt)
		if err != nil {
			failedTools[opt.Name] = err
			continue
		}

		storedEntries = removeEntry(storedEntries, entry)

		if status != nil {
			allStatus = append(allStatus, *status)
		}
	}

	// what remains is tools in the store that are not configured
	// this can happen if the user configures a tool, installs it, then removes the configuration... in which case
	// the tool is still in the store but future actions will result in no action taken against it in the store.
	for _, entry := range storedEntries {
		names = append(names, entry.Name)
		installedVersion, isHashValid, err := getInstallationStatus(entry)
		if err != nil {
			failedTools[entry.Name] = err
			continue
		}
		allStatus = append(allStatus, toolStatus{
			Name:             entry.Name,
			WantedVersion:    "?",
			ResolvedVersion:  "",
			Constraint:       "",
			IsInstalled:      true,
			HashIsValid:      isHashValid,
			InstalledVersion: installedVersion,
		})
	}

	// we weren't able to get status for all tools, but we should still present these
	for name, err := range failedTools {
		opt := cmdCfg.Core.Tools.GetOption(name)
		var wantVersion string
		if opt != nil {
			wantVersion = opt.Version.Want
		}
		allStatus = append(allStatus, toolStatus{
			Name:          name,
			WantedVersion: wantVersion,
			Error:         err,
		})
	}

	return allStatus
}

func getStatus(store *gobin.Store, opt option.Tool) (*toolStatus, *gobin.StoreEntry, error) {
	t, intent, err := opt.ToTool()
	if err != nil {
		return nil, nil, err
	}

	entries := store.GetByName(t.Name())
	if len(entries) > 1 {
		return nil, nil, gobin.ErrMultipleInstallations
	}

	var (
		isHashValid      bool
		installedVersion string
		isInstalled      = len(entries) == 1
		entry            *gobin.StoreEntry
	)

	if isInstalled {
		entry = &entries[0]

		installedVersion, isHashValid, err = getInstallationStatus(*entry)
		if err != nil {
			return nil, nil, err
		}
	}

	resolvedVersion, err := tool.ResolveVersion(t, *intent)
	if err != nil {
		return nil, nil, err
	}

	return &toolStatus{
		Name:             opt.Name,
		WantedVersion:    opt.Version.Want,
		ResolvedVersion:  resolvedVersion,
		Constraint:       opt.Version.Constraint,
		IsInstalled:      isInstalled,
		HashIsValid:      isHashValid,
		InstalledVersion: installedVersion,
	}, entry, nil
}

func getInstallationStatus(entry gobin.StoreEntry) (installedVersion string, isHashValid bool, err error) {
	installedVersion = entry.InstalledVersion

	err = entry.Verify(true, false)
	if err != nil {
		var errMismatch *gobin.ErrDigestMismatch
		if !errors.As(err, &errMismatch) {
			// TODO: bail if something more fundamental is wrong? or should we continue and note an error?
			return "", false, err
		}
		// TODO: should we show the mismatched hash on the UI?
	}
	isHashValid = err == nil
	err = nil
	return
}

func removeEntry(entries []gobin.StoreEntry, entry *gobin.StoreEntry) []gobin.StoreEntry {
	if entry == nil {
		return entries
	}
	for idx, e := range entries {
		if e.Name == entry.Name && e.InstalledVersion == entry.InstalledVersion {
			return append(entries[:idx], entries[idx+1:]...)
		}
	}
	return entries
}

func presentUpdates(statuses []toolStatus) error {
	t := table.NewWriter()
	t.SetStyle(table.StyleLight)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false

	titles := []string{
		"Tool", "Update",
	}

	var header table.Row
	for _, title := range titles {
		header = append(header, title)
	}
	t.AppendHeader(header)

	var rows []table.Row
	for _, status := range statuses {
		row := getToolUpdatesRow(status)
		if row != nil {
			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		bus.Report("all tools up to date")
		return nil
	}

	sort.Slice(rows, func(i, j int) bool {
		// sort by name
		return rows[i][0].(string) < rows[j][0].(string)
	})

	for _, row := range rows {
		t.AppendRow(row)
	}

	bus.Report(t.Render())
	return nil
}

func getToolUpdatesRow(item toolStatus) table.Row {
	var (
		commentary string
		style      lipgloss.Style
	)

	if item.Error != nil {
		commentary = item.Error.Error()
		style = badStatus
	} else {
		if !item.IsInstalled {
			commentary = "not installed"
		} else {
			switch {
			case item.WantedVersion == "?":
				commentary = ""
			case item.InstalledVersion != item.ResolvedVersion:
				commentary = fmt.Sprintf("%s → %s", summarizeVersion(item.InstalledVersion), summarizeVersion(item.ResolvedVersion))
			case !item.HashIsValid:
				commentary = ""
			}
		}
	}

	if commentary == "" {
		return nil
	}

	row := table.Row{
		item.Name,
		style.Render(commentary),
	}

	return row
}

func presentList(statuses []toolStatus) error {
	if len(statuses) == 0 {
		bus.Report("no tools configured or installed")
		return nil
	}
	t := table.NewWriter()
	t.SetStyle(table.StyleLight)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false

	// Fields:
	// Name, Wanted, Resolved, Constraint, Installed, Hash is valid, InstalledVersion

	// ok = is installed
	// ok &= resolved version == installed version
	// ok &= hash is valid

	// Column content
	// Name, Version (Resolved if not matching), [Constraint], Commentary if not valid

	var constraintNeeded bool
	for _, status := range statuses {
		if status.Constraint != "" {
			constraintNeeded = true
			break
		}
	}

	titles := []string{
		"Tool", "Desired Version", "Constraint", "",
	}

	var header table.Row
	for _, title := range titles {
		if title == "Constraint" && !constraintNeeded {
			continue
		}
		header = append(header, title)
	}
	t.AppendHeader(header)

	var rows []table.Row
	for _, status := range statuses {
		rows = append(rows, getToolStatusRow(status, constraintNeeded))
	}

	sort.Slice(rows, func(i, j int) bool {
		// sort by name
		return rows[i][0].(string) < rows[j][0].(string)
	})

	for _, row := range rows {
		t.AppendRow(row)
	}

	bus.Report(t.Render())
	return nil
}

func getToolStatusRow(item toolStatus, constraintNeeded bool) table.Row {
	var (
		commentary string
		severity   int
	)

	if item.Error != nil {
		commentary = item.Error.Error()
		severity = 2
	} else {
		if !item.IsInstalled {
			commentary = "not installed"
			severity = 1
		} else {
			switch {
			case item.WantedVersion == "?":
				commentary = "tool is not configured"
				severity = 2
			case item.InstalledVersion != item.ResolvedVersion:
				commentary = fmt.Sprintf("installed version (%s) does not match resolved version (%s)", summarizeVersion(item.InstalledVersion), summarizeVersion(item.ResolvedVersion))
				severity = 1
			case !item.HashIsValid:
				commentary = "hash is invalid"
				severity = 2
			}
		}
	}

	version := item.WantedVersion

	if item.WantedVersion != item.ResolvedVersion && item.ResolvedVersion != "" {
		version += fmt.Sprintf(" (%s)", summarizeVersion(item.ResolvedVersion))
	}

	style := toolStatusStyle(severity)

	row := table.Row{
		item.Name,
		style.Render(version),
	}

	if constraintNeeded {
		row = append(row, item.Constraint)
	}

	row = append(row, style.Render(commentary))

	return row
}

func summarizeVersion(v string) string {
	// TODO: there are probably better ways to do this
	// if it looks like a git hash, then summarize it
	if len(v) == 40 {
		return v[:7]
	}
	return v
}

var (
	goodStatus      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))  // 10 = high intensity green (ANSI 16 bit color code)
	badStatus       = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // 214 = orange1 (ANSI 16 bit color code)
	reallyBadStatus = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))   // 9 = high intensity red (ANSI 16 bit color code)
)

func toolStatusStyle(severity int) lipgloss.Style {
	switch severity {
	case 0:
		return goodStatus
	case 1:
		return badStatus
	}

	return reallyBadStatus
}
