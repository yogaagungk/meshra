package relay

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/connectors"
	"meshra/internal/model"
)

// fixtureTransport is a local provider double. It never opens a network
// connection and never contacts a real provider.
type fixtureTransport struct {
	kind   connectors.Kind
	events chan connectors.Event
	mu     sync.Mutex
	posts  []fixturePost
	fail   error
}

// fixturePost is one recorded outbound reply.
type fixturePost struct {
	ThreadKey string
	Message   connectors.Message
}

func (f *fixtureTransport) Kind() connectors.Kind { return f.kind }

func (f *fixtureTransport) Receive(ctx context.Context, sink connectors.Sink) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-f.events:
			if !sink(event) {
				return nil
			}
		}
	}
}

func (f *fixtureTransport) Post(_ context.Context, threadKey string, message connectors.Message) (connectors.PostResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return connectors.PostResult{}, f.fail
	}
	f.posts = append(f.posts, fixturePost{ThreadKey: threadKey, Message: message})
	return connectors.PostResult{RemoteID: "remote-" + threadKey}, nil
}

func (f *fixtureTransport) Download(_ context.Context, file connectors.File) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("fixture:" + file.ID)), nil
}

func (f *fixtureTransport) Test(context.Context) error { return nil }
func (f *fixtureTransport) Close() error              { return nil }

// recorded returns the replies the fixture received.
func (f *fixtureTransport) recorded() []fixturePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fixturePost{}, f.posts...)
}

// applyConnectorMigration creates the connector tables in a test database.
func applyConnectorMigration(t *testing.T, r *Relay) {
	t.Helper()
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='messaging_connectors'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		return
	}
	for _, statement := range strings.Split(connectorMigration, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" || strings.HasPrefix(statement, "UPDATE schema_version") {
			continue
		}
		if _, err := r.db.Exec(statement); err != nil {
			t.Fatalf("connector migration statement failed: %v", err)
		}
	}
}

// connectorFixture opens a relay whose provider transport is the local double.
func connectorFixture(t *testing.T) (*Relay, string, *fixtureTransport) {
	t.Helper()
	r, dir := continuityOpenTest(t, nil)
	applyConnectorMigration(t, r)
	fixture := &fixtureTransport{kind: connectors.KindSlack, events: make(chan connectors.Event, 16)}
	r.SetConnectorTransportFactory(func(context.Context, connectors.Config) (connectors.Transport, error) {
		return fixture, nil
	})
	return r, dir, fixture
}

// connectorCall invokes one connector operation as the operator.
func connectorCall(t *testing.T, r *Relay, operation string, payload any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.connectorOperation(context.Background(), admin, operation, raw)
}

// connectorViewOf reads the current summary of one connector.
func connectorViewOf(t *testing.T, r *Relay, id string) connectorView {
	t.Helper()
	value, err := connectorCall(t, r, "connectors.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	views, ok := value.([]connectorView)
	if !ok {
		t.Fatalf("unexpected connector list: %T", value)
	}
	for _, view := range views {
		if view.ID == id {
			return view
		}
	}
	t.Fatalf("connector %s is missing", id)
	return connectorView{}
}

// connectorSaved stores one connector with its credential.
func connectorSaved(t *testing.T, r *Relay, kind, token string) connectorView {
	t.Helper()
	secrets := map[string]string{"token": token}
	if kind == "slack" {
		secrets["appToken"] = token + "-app"
	}
	value, err := connectorCall(t, r, "connectors.save", map[string]any{"connector": map[string]any{"kind": kind, "name": "Fixture connector"}, "secrets": secrets})
	if err != nil {
		t.Fatal(err)
	}
	return value.(connectorView)
}

// connectorRouted stores one reviewed route for a connector.
func connectorRouted(t *testing.T, r *Relay, id string, profileID string, allow map[string]any, autoStart bool) connectorView {
	t.Helper()
	view := connectorViewOf(t, r, id)
	value, err := connectorCall(t, r, "connectors.routesSave", map[string]any{"id": id, "expectedRevision": view.Revision, "routes": []map[string]any{{"profileId": profileID, "allow": allow, "autoStart": autoStart, "enabled": true}}})
	if err != nil {
		t.Fatal(err)
	}
	return value.(connectorView)
}

// connectorEnabled enables or disables one connector.
func connectorEnabled(t *testing.T, r *Relay, id string, enabled bool) connectorView {
	t.Helper()
	view := connectorViewOf(t, r, id)
	value, err := connectorCall(t, r, "connectors.setEnabled", map[string]any{"id": id, "expectedRevision": view.Revision, "enabled": enabled})
	if err != nil {
		t.Fatal(err)
	}
	return value.(connectorView)
}

// connectorTask waits for the task admitted by one inbound event.
func connectorTask(t *testing.T, r *Relay, connectorID, eventID string) model.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := r.db.QueryRow("SELECT task_id FROM connector_inbox WHERE connector_id=? AND event_id=?", connectorID, eventID).Scan(&id)
		if err == nil && id != "" {
			return awaitTask(t, r, id)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no task was admitted for event %s", eventID)
	return model.Task{}
}

// connectorTaskCount counts the tasks admitted through one connector.
func connectorTaskCount(t *testing.T, r *Relay, connectorID string) int {
	t.Helper()
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM connector_inbox WHERE connector_id=? AND task_id<>''", connectorID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// connectorDeliveryStates reads the state of every recorded delivery.
func connectorDeliveryStates(t *testing.T, r *Relay, connectorID string) map[string]string {
	t.Helper()
	rows, err := r.db.Query("SELECT id,state FROM connector_outbox WHERE json_extract(body,'$.connectorId')=?", connectorID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var id, state string
		if err = rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		states[id] = state
	}
	return states
}

func TestConnectorDuplicateEventAcrossRestartAdmitsOneTask(t *testing.T) {
	r, dir, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0001")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	event := connectors.Event{Kind: connectors.KindSlack, EventID: "Ev100", ThreadKey: "C100:1700000000.0001", ChannelID: "C100", WorkspaceID: "T100", AuthorID: "U100", Text: "status please"}
	fixture.events <- event
	first := connectorTask(t, r, connector.ID, "Ev100")
	if first.State != "completed" {
		t.Fatalf("connector task did not complete: %+v", first)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dir, fakeAdapters(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	applyConnectorMigration(t, restarted)
	second := &fixtureTransport{kind: connectors.KindSlack, events: make(chan connectors.Event, 16)}
	restarted.SetConnectorTransportFactory(func(context.Context, connectors.Config) (connectors.Transport, error) {
		return second, nil
	})
	if err = restarted.StartConnectors(context.Background()); err != nil {
		t.Fatal(err)
	}
	second.events <- event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if connectorTaskCount(t, restarted, connector.ID) > 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if count := connectorTaskCount(t, restarted, connector.ID); count != 1 {
		t.Fatalf("duplicate event admitted %d tasks", count)
	}
}

func TestConnectorScopeBotEchoAndDisabledAdmitNothing(t *testing.T) {
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0002")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	// The connector is not enabled yet: no event may be admitted.
	r.connectorIngress(context.Background(), connector.ID, connectors.Event{Kind: connectors.KindSlack, EventID: "Ev200", ThreadKey: "C100:1700000000.0002", ChannelID: "C100", AuthorID: "U100", Text: "too early"})
	if count := connectorTaskCount(t, r, connector.ID); count != 0 {
		t.Fatalf("a disabled connector admitted %d tasks", count)
	}
	connectorEnabled(t, r, connector.ID, true)
	// A user outside the reviewed scope creates no task.
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev201", ThreadKey: "C999:1700000000.0003", ChannelID: "C999", WorkspaceID: "T100", AuthorID: "U999", Text: "not allowed"}
	time.Sleep(300 * time.Millisecond)
	if count := connectorTaskCount(t, r, connector.ID); count != 0 {
		t.Fatalf("a denied scope admitted %d tasks", count)
	}
	// A bot echo is ignored even when it arrives from an allowed channel.
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev202", ThreadKey: "C100:1700000000.0004", ChannelID: "C100", AuthorID: "UBOT", Text: "echo", Bot: true}
	time.Sleep(300 * time.Millisecond)
	if count := connectorTaskCount(t, r, connector.ID); count != 0 {
		t.Fatalf("a bot echo admitted %d tasks", count)
	}
	// An allowed message is admitted exactly once.
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev203", ThreadKey: "C100:1700000000.0005", ChannelID: "C100", WorkspaceID: "T100", AuthorID: "U100", Text: "hello"}
	task := connectorTask(t, r, connector.ID, "Ev203")
	if task.State != "completed" || task.ConversationID == "" {
		t.Fatalf("allowed message did not complete: %+v", task)
	}
	// Disabling the connector stops new admissions.
	connectorEnabled(t, r, connector.ID, false)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev204", ThreadKey: "C100:1700000000.0006", ChannelID: "C100", AuthorID: "U100", Text: "after disable"}
	time.Sleep(300 * time.Millisecond)
	if count := connectorTaskCount(t, r, connector.ID); count != 1 {
		t.Fatalf("a disabled connector admitted %d tasks", count)
	}
}

func TestConnectorDeliveryUncertainNeverRerunsTheTask(t *testing.T) {
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0003")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev300", ThreadKey: "C100:1700000000.0007", ChannelID: "C100", AuthorID: "U100", Text: "reply to me"}
	task := connectorTask(t, r, connector.ID, "Ev300")
	if task.State != "completed" {
		t.Fatalf("task did not complete: %+v", task)
	}
	if err := r.connectorCollectReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	states := connectorDeliveryStates(t, r, connector.ID)
	if len(states) != 1 {
		t.Fatalf("expected one recorded reply, got %v", states)
	}
	var deliveryID string
	for id := range states {
		deliveryID = id
	}
	// The provider call fails in an uncertain way: the reply may or may not
	// have been accepted.
	fixture.mu.Lock()
	fixture.fail = connectors.Uncertain("provider connection dropped")
	fixture.mu.Unlock()
	if err := r.connectorDeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if states := connectorDeliveryStates(t, r, connector.ID); states[deliveryID] != "uncertain_delivery" {
		t.Fatalf("uncertain delivery was recorded as %v", states)
	}
	if count := connectorTaskCount(t, r, connector.ID); count != 1 {
		t.Fatalf("an uncertain delivery reran the task: %d admissions", count)
	}
	if _, err := connectorCall(t, r, "connectors.retryDelivery", map[string]any{"deliveryId": deliveryID}); err == nil {
		t.Fatal("a retry without duplicate risk confirmation was accepted")
	} else {
		requireStatus(t, err, 400)
	}
	if _, err := connectorCall(t, r, "connectors.retryDelivery", map[string]any{"deliveryId": deliveryID, "confirmDuplicateRisk": true}); err != nil {
		t.Fatal(err)
	}
	// A crash before the provider call leaves the delivery uncertain as well.
	if err := r.connectorRecover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count := connectorTaskCount(t, r, connector.ID); count != 1 {
		t.Fatalf("a restart reran the task: %d admissions", count)
	}
	// A confirmed retry posts the reply exactly once more.
	fixture.mu.Lock()
	fixture.fail = nil
	fixture.mu.Unlock()
	if err := r.connectorDeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if states := connectorDeliveryStates(t, r, connector.ID); states[deliveryID] != "sent" {
		t.Fatalf("confirmed retry did not deliver: %v", states)
	}
	posts := fixture.recorded()
	if len(posts) != 1 || posts[0].ThreadKey != "C100:1700000000.0007" {
		t.Fatalf("unexpected recorded replies: %+v", posts)
	}
	if !strings.Contains(posts[0].Message.Text, "reply:") {
		t.Fatalf("the recorded reply does not carry the task result: %q", posts[0].Message.Text)
	}
}

func TestConnectorInterruptedDeliveryBecomesUncertain(t *testing.T) {
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", "xoxb-fixture-token-0004")
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev400", ThreadKey: "C100:1700000000.0008", ChannelID: "C100", AuthorID: "U100", Text: "interrupted"}
	task := connectorTask(t, r, connector.ID, "Ev400")
	if task.State != "completed" {
		t.Fatalf("task did not complete: %+v", task)
	}
	if err := r.connectorCollectReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The daemon persisted the sending state and stopped before the provider
	// confirmed the reply.
	if _, err := r.db.Exec("UPDATE connector_outbox SET state='sending'"); err != nil {
		t.Fatal(err)
	}
	if err := r.connectorRecover(context.Background()); err != nil {
		t.Fatal(err)
	}
	states := connectorDeliveryStates(t, r, connector.ID)
	for id, state := range states {
		if state != "uncertain_delivery" {
			t.Fatalf("interrupted delivery %s is %s", id, state)
		}
	}
	if count := connectorTaskCount(t, r, connector.ID); count != 1 {
		t.Fatalf("an interrupted delivery reran the task: %d admissions", count)
	}
	if len(fixture.recorded()) != 0 {
		t.Fatal("an interrupted delivery posted a reply")
	}
}

func TestConnectorCredentialsNeverAppearInViewsEventsOrRows(t *testing.T) {
	secret := "xoxb-super-secret-connector-token-9911"
	r, _, fixture := connectorFixture(t)
	profile := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	connector := connectorSaved(t, r, "slack", secret)
	connectorRouted(t, r, connector.ID, profile.ID, map[string]any{"channels": []string{"C100"}}, true)
	connectorEnabled(t, r, connector.ID, true)
	fixture.events <- connectors.Event{Kind: connectors.KindSlack, EventID: "Ev500", ThreadKey: "C100:1700000000.0009", ChannelID: "C100", AuthorID: "U100", Text: "hello"}
	connectorTask(t, r, connector.ID, "Ev500")
	if err := r.connectorCollectReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.connectorDeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := connectorViewOf(t, r, connector.ID)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the connector view exposed a credential: %s", encoded)
	}
	if view.Secrets["token"] != "saved" || view.Secrets["appToken"] != "saved" {
		t.Fatalf("unexpected credential status: %+v", view.Secrets)
	}
	rows, err := r.db.Query("SELECT data FROM automation_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, secret) {
			t.Fatalf("an automation event exposed a credential: %s", body)
		}
	}
	for _, table := range []string{"connector_inbox", "connector_outbox", "messaging_connectors", "connector_routes"} {
		rows, err = r.db.Query("SELECT body FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var body string
			if err = rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body, secret) {
				t.Fatalf("%s exposed a credential: %s", table, body)
			}
		}
		rows.Close()
	}
}

func TestConnectorOperationsRequireOperatorAuthority(t *testing.T) {
	r, _, _ := connectorFixture(t)
	raw, err := json.Marshal(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.connectorOperation(context.Background(), model.Principal{AgentID: "omp:worker"}, "connectors.list", raw); err == nil {
		t.Fatal("a non operator listed the connectors")
	} else {
		requireStatus(t, err, 403)
	}
	if _, err = connectorCall(t, r, "connectors.save", map[string]any{"connector": map[string]any{"kind": "unknown", "name": "bad"}}); err == nil {
		t.Fatal("an unsupported connector kind was accepted")
	} else {
		requireStatus(t, err, 400)
	}
	if _, err = connectorCall(t, r, "connectors.save", map[string]any{"connector": map[string]any{"kind": "slack", "name": "bad"}, "secrets": map[string]string{"token": "value", "unknown": "value"}}); err == nil {
		t.Fatal("an unsupported credential name was accepted")
	} else {
		requireStatus(t, err, 400)
	}
	if _, err = connectorCall(t, r, "connectors.setEnabled", map[string]any{"id": "missing", "expectedRevision": 1, "enabled": true}); err == nil {
		t.Fatal("an unknown connector was enabled")
	} else {
		requireStatus(t, err, 404)
	}
}
