//#nosec G404
package core_test

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"testing"
	"time"

	"github.com/dosco/graphjin/core"
	"github.com/goccy/go-json"
	"golang.org/x/sync/errgroup"
)

var cursorRegex *regexp.Regexp

func init() {
	cursorRegex, _ = regexp.Compile(`cursor\"\:\"([^\s\"]+)`)
}

func Example_subscription() {
	gql := `subscription test {
		users(id: $id) {
			id
			email
			phone
		}
	}`

	vars := json.RawMessage(`{ "id": 3 }`)

	conf := newConfig(&core.Config{DBType: dbType, DisableAllowList: true, SubsPollDuration: 1})
	gj, err := core.NewGraphJin(conf, pool)
	if err != nil {
		panic(err)
	}

	m, err := gj.Subscribe(context.Background(), gql, vars, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	for i := 0; i < 10; i++ {
		msg := <-m.Result
		fmt.Println(string(msg.Data))

		// update user phone in database to trigger subscription
		q := fmt.Sprintf(`UPDATE users SET phone = '650-447-000%d' WHERE id = 3`, i)
		if _, err := pool.Exec(context.Background(), q); err != nil {
			panic(err)
		}
	}

	// Output:
	// {"users": {"id": 3, "email": "user3@test.com", "phone": null}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0000"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0001"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0002"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0003"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0004"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0005"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0006"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0007"}}
	// {"users": {"id": 3, "email": "user3@test.com", "phone": "650-447-0008"}}
}

func Example_subscriptionWithCursor() {
	// query to fetch existing chat messages
	// gql1 := `query {
	// 	chats(first: 3, after: $cursor) {
	// 		id
	// 		body
	// 	}
	// 	chats_cursor
	// }`

	// query to subscribe to new chat messages
	gql2 := `subscription {
		chats(first: 1, after: $cursor) {
			id
			body
		}
	}`

	conf := newConfig(&core.Config{DBType: dbType, DisableAllowList: true, SubsPollDuration: 1})
	gj, err := core.NewGraphJin(conf, pool)
	if err != nil {
		panic(err)
	}

	// struct to hold the cursor value from fetching the existing
	// chat messages
	// res := struct {
	// 	Cursor string `json:"chats_cursor"`
	// }{}

	// execute query for existing chat messages
	// m1, err := gj.GraphQL(context.Background(), gql1, nil, nil)
	// if err != nil {
	// 	fmt.Println(err)
	// 	return
	// }

	// extract out the cursor `chats_cursor` to use in the subscription
	// if err := json.Unmarshal(m1.Data, &res); err != nil {
	// 	fmt.Println(err)
	// 	return
	// }

	// create variables with the previously extracted cursor value to
	// pass to the new chat messages subscription
	// vars := json.RawMessage(`{ "cursor": "` + res.Cursor + `" }`)
	vars := json.RawMessage(`{ "cursor": null }`)

	// subscribe to new chat messages using the cursor
	m2, err := gj.Subscribe(context.Background(), gql2, vars, nil)
	if err != nil {
		fmt.Println(err)
		return
	}

	go func() {
		for i := 6; i < 20; i++ {
			// insert a new chat message
			q := fmt.Sprintf(`INSERT INTO chats (id, body) VALUES (%d, 'New chat message %d')`, i, i)
			if _, err := pool.Exec(context.Background(), q); err != nil {
				panic(err)
			}
			time.Sleep(3 * time.Second)
		}
	}()

	for i := 0; i < 19; i++ {
		msg := <-m2.Result
		fmt.Println(string(msg.Data))
	}

	// Output:
	// {"chats": [{"id": 1, "body": "This is chat message number 1"}], "chats_cursor": "1"}
	// {"chats": [{"id": 2, "body": "This is chat message number 2"}], "chats_cursor": "2"}
	// {"chats": [{"id": 3, "body": "This is chat message number 3"}], "chats_cursor": "3"}
	// {"chats": [{"id": 4, "body": "This is chat message number 4"}], "chats_cursor": "4"}
	// {"chats": [{"id": 5, "body": "This is chat message number 5"}], "chats_cursor": "5"}
	// {"chats": [{"id": 6, "body": "New chat message 6"}], "chats_cursor": "6"}
	// {"chats": [{"id": 7, "body": "New chat message 7"}], "chats_cursor": "7"}
	// {"chats": [{"id": 8, "body": "New chat message 8"}], "chats_cursor": "8"}
	// {"chats": [{"id": 9, "body": "New chat message 9"}], "chats_cursor": "9"}
	// {"chats": [{"id": 10, "body": "New chat message 10"}], "chats_cursor": "10"}
	// {"chats": [{"id": 11, "body": "New chat message 11"}], "chats_cursor": "11"}
	// {"chats": [{"id": 12, "body": "New chat message 12"}], "chats_cursor": "12"}
	// {"chats": [{"id": 13, "body": "New chat message 13"}], "chats_cursor": "13"}
	// {"chats": [{"id": 14, "body": "New chat message 14"}], "chats_cursor": "14"}
	// {"chats": [{"id": 15, "body": "New chat message 15"}], "chats_cursor": "15"}
	// {"chats": [{"id": 16, "body": "New chat message 16"}], "chats_cursor": "16"}
	// {"chats": [{"id": 17, "body": "New chat message 17"}], "chats_cursor": "17"}
	// {"chats": [{"id": 18, "body": "New chat message 18"}], "chats_cursor": "18"}
	// {"chats": [{"id": 19, "body": "New chat message 19"}], "chats_cursor": "19"}
}

func TestSubscription(t *testing.T) {
	gql := `subscription test {
		users(where: { or: { id: { eq: $id }, id: { eq: $id2 } } }) @object {
			id
			email
		}
	}`

	conf := newConfig(&core.Config{DBType: dbType, DisableAllowList: true, SubsPollDuration: 1})
	gj, err := core.NewGraphJin(conf, pool)
	if err != nil {
		panic(err)
	}

	g, ctx := errgroup.WithContext(context.Background())

	for i := 101; i < 3000; i++ {
		n := i
		time.Sleep(20 * time.Millisecond)

		g.Go(func() error {
			id := (rand.Intn(100-1) + 1)
			vars := json.RawMessage(fmt.Sprintf(`{ "id": %d, "id2": %d }`, n, id))
			m, err := gj.Subscribe(ctx, gql, vars, nil)
			if err != nil {
				return fmt.Errorf("subscribe: %w", err)
			}

			msg := <-m.Result
			exp := fmt.Sprintf(`{"users": {"id": %d, "email": "user%d@test.com"}}`, id, id)
			val := string(msg.Data)

			if val != exp {
				t.Errorf("expected '%s' got '%s'", exp, val)
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		panic(err)
	}
}
