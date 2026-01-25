package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// View state
type viewState int

const (
	viewList viewState = iota
	viewDetail
)

// KeyMap defines the keybindings
type KeyMap struct {
	Up          key.Binding
	Down        key.Binding
	Enter       key.Binding
	Escape      key.Binding
	ApproveOnce key.Binding
	ApproveGen  key.Binding
	Ignore      key.Binding
	Freeze      key.Binding
	Snooze      key.Binding
	Refresh     key.Binding
	Quit        key.Binding
}

// DefaultKeyMap returns the default keybindings
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "details"),
		),
		Escape: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		ApproveOnce: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "approve once"),
		),
		ApproveGen: key.NewBinding(
			key.WithKeys("A"),
			key.WithHelp("A", "approve generation"),
		),
		Ignore: key.NewBinding(
			key.WithKeys("i"),
			key.WithHelp("i", "ignore always"),
		),
		Freeze: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "freeze"),
		),
		Snooze: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "snooze 1h"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "refresh"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
	}
}

// ShortHelp returns keybindings for short help
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.ApproveOnce, k.Snooze, k.Quit}
}

// FullHelp returns keybindings for extended help
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Escape},
		{k.ApproveOnce, k.ApproveGen, k.Ignore},
		{k.Freeze, k.Snooze, k.Refresh, k.Quit},
	}
}

// Model is the bubbletea model for the CLI
type Model struct {
	client *Client
	items  []DriftItem
	cursor int
	view   viewState
	keys   KeyMap
	help   help.Model
	width  int
	height int
	status string
	err    error
}

// NewModel creates a new CLI model
func NewModel(client *Client) Model {
	return Model{
		client: client,
		items:  []DriftItem{},
		cursor: 0,
		view:   viewList,
		keys:   DefaultKeyMap(),
		help:   help.New(),
		status: "Loading...",
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return m.loadDrifts
}

// Messages
type driftsLoadedMsg struct {
	items []DriftItem
}

type actionDoneMsg struct {
	action string
	err    error
}

type errMsg struct {
	err error
}

func (m Model) loadDrifts() tea.Msg {
	// This would need to be configured with the GVK to watch
	// For now, return empty - the real implementation will use the client
	return driftsLoadedMsg{items: m.items}
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.Width = msg.Width
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case driftsLoadedMsg:
		m.items = msg.items
		m.status = fmt.Sprintf("%d drift(s) found", len(m.items))
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.status = fmt.Sprintf("Error: %v", msg.err)
		} else {
			m.status = fmt.Sprintf("Action '%s' completed", msg.action)
		}
		return m, m.loadDrifts

	case errMsg:
		m.err = msg.err
		m.status = fmt.Sprintf("Error: %v", msg.err)
		return m, nil
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle escape in detail view
	if m.view == viewDetail {
		if key.Matches(msg, m.keys.Escape) {
			m.view = viewList
			return m, nil
		}
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
		return m, nil

	case key.Matches(msg, m.keys.Enter):
		if len(m.items) > 0 {
			m.view = viewDetail
		}
		return m, nil

	case key.Matches(msg, m.keys.Escape):
		m.view = viewList
		return m, nil

	case key.Matches(msg, m.keys.ApproveOnce):
		if len(m.items) > 0 {
			return m, m.approveOnce(m.items[m.cursor])
		}

	case key.Matches(msg, m.keys.ApproveGen):
		if len(m.items) > 0 {
			return m, m.approveGeneration(m.items[m.cursor])
		}

	case key.Matches(msg, m.keys.Ignore):
		if len(m.items) > 0 {
			return m, m.ignore(m.items[m.cursor])
		}

	case key.Matches(msg, m.keys.Freeze):
		if len(m.items) > 0 {
			return m, m.freeze(m.items[m.cursor])
		}

	case key.Matches(msg, m.keys.Snooze):
		if len(m.items) > 0 {
			return m, m.snooze(m.items[m.cursor])
		}

	case key.Matches(msg, m.keys.Refresh):
		return m, m.loadDrifts
	}

	return m, nil
}

// Action commands
func (m Model) approveOnce(item DriftItem) tea.Cmd {
	return func() tea.Msg {
		err := m.client.ApproveOnce(context.Background(), item)
		return actionDoneMsg{action: "approve once", err: err}
	}
}

func (m Model) approveGeneration(item DriftItem) tea.Cmd {
	return func() tea.Msg {
		err := m.client.ApproveGeneration(context.Background(), item)
		return actionDoneMsg{action: "approve generation", err: err}
	}
}

func (m Model) ignore(item DriftItem) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Ignore(context.Background(), item)
		return actionDoneMsg{action: "ignore", err: err}
	}
}

func (m Model) freeze(item DriftItem) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Freeze(context.Background(), item, "frozen via CLI")
		return actionDoneMsg{action: "freeze", err: err}
	}
}

func (m Model) snooze(item DriftItem) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Snooze(context.Background(), item, 1*time.Hour, "", "")
		return actionDoneMsg{action: "snooze 1h", err: err}
	}
}

// View renders the UI
func (m Model) View() string {
	if m.view == viewDetail {
		return m.viewDetailPage()
	}
	return m.viewListPage()
}

func (m Model) viewListPage() string {
	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Kausality Drift Monitor"))
	b.WriteString("\n\n")

	// Items
	if len(m.items) == 0 {
		b.WriteString(itemStyle.Render("No drifts detected"))
		b.WriteString("\n")
	} else {
		for i, item := range m.items {
			cursor := "  "
			style := itemStyle
			if i == m.cursor {
				cursor = "> "
				style = selectedItemStyle
			}

			line := fmt.Sprintf("%s%s", cursor, item.Title())
			b.WriteString(style.Render(line))
			b.WriteString("\n")
			b.WriteString(itemStyle.Render("   " + item.Description()))
			b.WriteString("\n")
		}
	}

	// Status bar
	b.WriteString("\n")
	b.WriteString(statusBarStyle.Render(m.status))
	b.WriteString("\n")

	// Help
	b.WriteString(helpStyle.Render(m.help.View(m.keys)))

	return b.String()
}

func (m Model) viewDetailPage() string {
	if len(m.items) == 0 || m.cursor >= len(m.items) {
		return "No item selected"
	}

	item := m.items[m.cursor]

	var b strings.Builder

	// Title
	b.WriteString(modalTitleStyle.Render("Drift Details"))
	b.WriteString("\n\n")

	// Fields
	fields := []struct {
		label string
		value string
	}{
		{"ID", item.ID},
		{"Phase", item.Phase},
		{"", ""},
		{"Parent", fmt.Sprintf("%s/%s", item.ParentKind, item.ParentName)},
		{"Parent NS", item.ParentNamespace},
		{"Parent API", item.ParentAPIVersion},
		{"", ""},
		{"Child", fmt.Sprintf("%s/%s", item.ChildKind, item.ChildName)},
		{"Child NS", item.ChildNamespace},
		{"Child API", item.ChildAPIVersion},
		{"", ""},
		{"User", item.User},
		{"Operation", item.Operation},
		{"Detected At", item.DetectedAt.Format(time.RFC3339)},
	}

	for _, f := range fields {
		if f.label == "" {
			b.WriteString("\n")
			continue
		}
		line := lipgloss.JoinHorizontal(
			lipgloss.Left,
			labelStyle.Render(f.label+":"),
			valueStyle.Render(f.value),
		)
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("Press ESC to go back"))

	return modalStyle.Render(b.String())
}

// SetItems sets the drift items (used for external updates)
func (m *Model) SetItems(items []DriftItem) {
	m.items = items
	m.status = fmt.Sprintf("%d drift(s) found", len(items))
}
