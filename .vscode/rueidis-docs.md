================
CODE SNIPPETS
================
TITLE: Rueidis Pub/Sub Example
DESCRIPTION: Demonstrates setting up a Redis Pub/Sub client using rueidiscompat.Adapter. It includes subscribing to a channel, publishing messages, and receiving them in a loop.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidiscompat/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
	"strconv"
)

func main() {
ctx := context.Background()
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	rdb := rueidiscompat.NewAdapter(client)
	pubsub := rdb.Subscribe(ctx, "mychannel1")
	defer pubsub.Close()

	go func() {
		for i := 0; ; i++ {
			if err := rdb.Publish(ctx, "mychannel1", strconv.Itoa(i)).Err(); err != nil {
				panic(err)
			}
		}
	}()
	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Println(msg.Channel, msg.Payload)
	}
}
```

--------------------------------

TITLE: Common Redis Command Examples with Rueidis Parsers in Go
DESCRIPTION: Provides examples of common Redis commands (GET, MGET, SET, INCR, HGET, HMGET, HGETALL, EXPIRE, HEXPIRE, ZRANGE, ZRANK, ZSCORE, ZPOPMIN, SCARD, SMEMBERS, LINDEX, LPOP, SCAN, FT.SEARCH, GEOSEARCH) and their corresponding Rueidis response parsing methods in Go.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
// GET
client.Do(ctx, client.B().Get().Key("k").Build()).ToString()
client.Do(ctx, client.B().Get().Key("k").Build()).AsInt64()
// MGET
client.Do(ctx, client.B().Mget().Key("k1", "k2").Build()).ToArray()
// SET
client.Do(ctx, client.B().Set().Key("k").Value("v").Build()).Error()
// INCR
client.Do(ctx, client.B().Incr().Key("k").Build()).AsInt64()
// HGET
client.Do(ctx, client.B().Hget().Key("k").Field("f").Build()).ToString()
// HMGET
client.Do(ctx, client.B().Hmget().Key("h").Field("a", "b").Build()).ToArray()
// HGETALL
client.Do(ctx, client.B().Hgetall().Key("h").Build()).AsStrMap()
// EXPIRE
client.Do(ctx, client.B().Expire().Key("k").Seconds(1).Build()).AsInt64()
// HEXPIRE
client.Do(ctx, client.B().Hexpire().Key("h").Seconds(1).Fields().Numfields(2).Field("f1", "f2").Build()).AsIntSlice()
// ZRANGE
client.Do(ctx, client.B().Zrange().Key("k").Min("1").Max("2").Build()).AsStrSlice()
// ZRANK
client.Do(ctx, client.B().Zrank().Key("k").Member("m").Build()).AsInt64()
// ZSCORE
client.Do(ctx, client.B().Zscore().Key("k").Member("m").Build()).AsFloat64()
// ZRANGE
client.Do(ctx, client.B().Zrange().Key("k").Min("0").Max("-1").Build()).AsStrSlice()
client.Do(ctx, client.B().Zrange().Key("k").Min("0").Max("-1").Withscores().Build()).AsZScores()
// ZPOPMIN
client.Do(ctx, client.B().Zpopmin().Key("k").Build()).AsZScore()
client.Do(ctx, client.B().Zpopmin().Key("myzset").Count(2).Build()).AsZScores()
// SCARD
client.Do(ctx, client.B().Scard().Key("k").Build()).AsInt64()
// SMEMBERS
client.Do(ctx, client.B().Smembers().Key("k").Build()).AsStrSlice()
// LINDEX
client.Do(ctx, client.B().Lindex().Key("k").Index(0).Build()).ToString()
// LPOP
client.Do(ctx, client.B().Lpop().Key("k").Build()).ToString()
client.Do(ctx, client.B().Lpop().Key("k").Count(2).Build()).AsStrSlice()
// SCAN
client.Do(ctx, client.B().Scan().Cursor(0).Build()).AsScanEntry()
// FT.SEARCH
client.Do(ctx, client.B().FtSearch().Index("idx").Query("@f:v").Build()).AsFtSearch()
// GEOSEARCH
client.Do(ctx, client.B().Geosearch().Key("k").Fromlonlat(1, 1).Bybox(1).Height(1).Km().Build()).AsGeosearch()
```

--------------------------------

TITLE: Rueidis Pipeline Example
DESCRIPTION: Illustrates how to perform multiple Redis commands efficiently using pipelining with rueidiscompat.Adapter. Commands are sent to the server in batches, reducing network latency.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidiscompat/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
)

func main() {
ctx := context.Background()
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	rdb := rueidiscompat.NewAdapter(client)
	cmds, err := rdb.Pipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
		for i := 0; i < 100; i++ {
			pipe.Set(ctx, fmt.Sprintf("key%d", i), i, 0)
			pipe.Get(ctx, fmt.Sprintf("key%d", i))
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	for _, cmd := range cmds {
		fmt.Println(cmd.(*rueidiscompat.StringCmd).Val())
	}
}
```

--------------------------------

TITLE: Rueidis Lua Script Example
DESCRIPTION: Provides an example of executing a Lua script with rueidiscompat.Adapter. It defines a script to atomically increment a Redis key and demonstrates how to run it.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidiscompat/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
)

var incrBy = rueidiscompat.NewScript $(
	"local key = KEYS[1]\n"
	"local change = ARGV[1]\n"
	"local value = redis.call(\"GET\", key)\n"
	"if not value then\n"
	"  value = 0\n"
	"end\n"
	"value = value + change\n"
	"redis.call(\"SET\", key, value)\n"
	"return value\n"
`)

func main() {
ctx := context.Background()
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	rdb := rueidiscompat.NewAdapter(client)
	keys := []string{"my_counter"}
	values := []interface{}{"+1"}
	fmt.Println(incrBy.Run(ctx, rdb, keys, values...).Int())
}
```

--------------------------------

TITLE: Install rueidislimiter Go Module
DESCRIPTION: This command installs the rueidislimiter module for Go projects. It fetches the package from the specified GitHub repository.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidislimiter/README.md

LANGUAGE: bash
CODE:
```
go get github.com/redis/rueidis/rueidislimiter
```

--------------------------------

TITLE: Create Go Redis Client and Execute Basic Commands
DESCRIPTION: Demonstrates how to initialize a single-node Redis client using Rueidis, execute SET, GET, HSET, and HGETALL commands, and handle potential errors. It uses the command builder for type-safe command construction.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/rueidis"
)

func main() {
	// Single node connection
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
		Username:    "default",
		Password:    "secret",
		SelectDB:    0,
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()

	// SET with expiration
	err = client.Do(ctx, client.B().Set().Key("user:1000").Value("john").Ex(3600).Build()).Error()
	if err != nil {
		panic(err)
	}

	// GET
	val, err := client.Do(ctx, client.B().Get().Key("user:1000").Build()).ToString()
	if err != nil {
		panic(err)
	}
	fmt.Println("Value:", val) // Output: Value: john

	// HSET and HGETALL
	client.Do(ctx, client.B().Hset().Key("user:1001").FieldValue().
		FieldValue("name", "jane").
		FieldValue("age", "25").Build()).Error()

	fields, err := client.Do(ctx, client.B().Hgetall().Key("user:1001").Build()).AsStrMap()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Hash: %+v\n", fields) // Output: Hash: map[age:25 name:jane]
}
```

--------------------------------

TITLE: Lua Scripting: Create and Execute Scripts
DESCRIPTION: Demonstrates creating, executing, and managing Lua scripts with Rueidis. Includes examples for atomic operations, read-only scripts, and automatic command selection (EVALSHA/EVAL). Requires a running Redis instance.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"

	"github.com/redis/rueidis"
)

func main() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()

	// Create Lua script for atomic increment with max value
	script := rueidis.NewLuaScript(`
		local current = redis.call('GET', KEYS[1])
		local max = tonumber(ARGV[1])
		if not current then
			redis.call('SET', KEYS[1], 1)
			return 1
		end
		local val = tonumber(current)
		if val < max then
			redis.call('INCR', KEYS[1])
			return val + 1
		end
		return val
	`)

	// Execute script (uses EVALSHA, falls back to EVAL if needed)
	result := script.Exec(ctx, client, []string{"counter"}, []string{"10"})
	count, err := result.AsInt64()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Counter: %d\n", count)

	// Read-only script for replica routing
	readScript := rueidis.NewLuaScriptReadOnly(`
		return redis.call('GET', KEYS[1])
	`)

	val := readScript.Exec(ctx, client, []string{"mykey"}, nil)
	str, _ := val.ToString()
	fmt.Printf("Value: %s\n", str)
}
```

--------------------------------

TITLE: Rueidis Transaction Example
DESCRIPTION: Shows how to implement Redis transactions using `Watch` and `TxPipelined` with rueidiscompat.Adapter. This ensures atomic execution of a series of commands, retrying if optimistic locking fails.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidiscompat/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
)

func main() {
ctx := context.Background()
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	key := "my_counter"
	rdb := rueidiscompat.NewAdapter(client)
	txf := func(tx rueidiscompat.Tx) error {
		n, err := tx.Get(ctx, key).Int()
		if err != nil && err != rueidiscompat.Nil {
			return err
		}
		// Operation is committed only if the watched keys remain unchanged.
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, key, n+1, 0)
			return nil
		})
		return err
	}
	for {
		err := rdb.Watch(ctx, txf, key)
		if err == nil {
			break
		} else if err == rueidiscompat.TxFailedErr {
			// Optimistic lock lost. Retry if the key has been changed.
			continue
		}
		panic(err)
	}
}
```

--------------------------------

TITLE: Rueidis Client-Side Caching Example
DESCRIPTION: Demonstrates how to use client-side caching with the rueidiscompat.Adapter by chaining the Cache(ttl) call before supported commands. This allows for faster data retrieval by caching responses locally.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidiscompat/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"time"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
)

func main() {
ctx := context.Background()
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	compat := rueidiscompat.NewAdapter(client)
	ok, _ := compat.SetNX(ctx, "key", "val", time.Second).Result()

	// with client side caching
	res, _ := compat.Cache(time.Second).Get(ctx, "key").Result()
}
```

--------------------------------

TITLE: MSet and MSetNX for batch writes in Go
DESCRIPTION: Provides examples for `rueidis.MSet` and `rueidis.MSetNX` to perform batch write operations. `MSet` sets multiple key-value pairs, returning errors for any keys that failed. `MSetNX` attempts to set multiple keys only if none of them already exist, returning errors for keys that already exist or other issues.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
ctx := context.Background()

// MSet: set multiple keys
kvs := map[string]string{
	"config:timeout": "30",
	"config:retries": "3",
	"config:debug":   "true",
}
errors := rueidis.MSet(client, ctx, kvs)
for key, err := range errors {
	if err != nil {
		fmt.Printf("Failed to set %s: %v\n", key, err)
	}
}

// MSetNX: set only if none exist
kvsNew := map[string]string{
	"lock:resource1": "instance1",
	"lock:resource2": "instance2",
}
errors = rueidis.MSetNX(client, ctx, kvsNew)
for key, err := range errors {
	if err != nil {
		fmt.Printf("Key %s already exists or error: %v\n", key, err)
	}
}

```

--------------------------------

TITLE: Configure Rueidis Client Using URLs
DESCRIPTION: This Go code snippet shows how to initialize a Rueidis Redis client using URL-based configurations. It covers connecting to a single node, a cluster with multiple nodes, a Redis Sentinel setup, and establishing a TLS connection. The `MustParseURL` function simplifies connection string management.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
// Single node
client, err := rueidis.NewClient(
	rueidis.MustParseURL("redis://user:pass@127.0.0.1:6379/0?dial_timeout=5s"))

// Cluster with multiple nodes
client, err = rueidis.NewClient(
	rueidis.MustParseURL("redis://127.0.0.1:7001?addr=127.0.0.1:7002&addr=127.0.0.1:7003"))

// Sentinel
client, err = rueidis.NewClient(
	rueidis.MustParseURL("redis://127.0.0.1:26379?master_set=mymaster"))

// TLS connection
client, err = rueidis.NewClient(
	rueidis.MustParseURL("rediss://127.0.0.1:6380"))

```

--------------------------------

TITLE: Golang: Manual Pipelining with DoMulti in Rueidis
DESCRIPTION: This Go code example shows how to manually pipeline multiple Redis commands using `DoMulti()` in rueidis. It constructs a slice of commands, executes them in a single batch, and iterates through the responses, handling potential errors. Commands are recycled by default, but `Pin()` can be used to preserve them.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
cmds := make(rueidis.Commands, 0, 10)
for i := 0; i < 10; i++ {
    cmds = append(cmds, client.B().Set().Key("key").Value("value").Build())
}
for _, resp := range client.DoMulti(ctx, cmds...) {
    if err := resp.Error(); err != nil {
        panic(err)
    }
}
```

--------------------------------

TITLE: Configure Go Redis Sentinel Client
DESCRIPTION: Illustrates how to create a Rueidis client that connects to a Redis Sentinel setup. This configuration is used for high availability, allowing the client to discover the current master node.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
client, err := rueidis.NewClient(rueidis.ClientOption{
	InitAddress: []string{"127.0.0.1:26379", "127.0.0.1:26380", "127.0.0.1:26381"},
	Sentinel: rueidis.SentinelOption{
		MasterSet: "mymaster",
		Username:  "sentinel_user",
		Password:  "sentinel_pass",
	},
})
```

--------------------------------

TITLE: Golang: Client-Side Caching with DoCache and DoMultiCache in Rueidis
DESCRIPTION: This Go code showcases how to enable server-assisted client-side caching in rueidis using `DoCache()` and `DoMultiCache()`. It demonstrates specifying client-side Time-To-Live (TTL) values for cached responses. The examples include caching a single `HMGET` command and multiple `GET` commands with different TTLs.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
client.DoCache(ctx, client.B().Hmget().Key("mk").Field("1", "2").Cache(), time.Minute).ToArray()
client.DoMultiCache(ctx,
    rueidis.CT(client.B().Get().Key("k1").Cache(), 1*time.Minute),
    rueidis.CT(client.B().Get().Key("k2").Cache(), 2*time.Minute))
```

--------------------------------

TITLE: Vector Similarity Search Queries with Rueidis Go
DESCRIPTION: Demonstrates constructing queries for vector similarity search using `rueidis.VectorString32` and `rueidis.VectorString64`. This example shows how to build a `FT.SEARCH` command with a KNN query and parameter binding for vector data.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
cmd := client.B().FtSearch().Index("idx").Query("*=>[KNN 5 @vec $V]").
    Params().Nargs(2).NameValue().NameValue("V", rueidis.VectorString64([]float64{...})).
    Dialect(2).Build()
n, resp, err := client.Do(ctx, cmd).AsFtSearch()
```

--------------------------------

TITLE: Golang: Using MGet Helper in Rueidis
DESCRIPTION: This Go example demonstrates the usage of the `MGet` helper function in rueidis for retrieving multiple keys efficiently. It shows how to pass a slice of keys to `MGet` and then access the corresponding values from the returned map, printing the value for a specific key.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
val, err := MGet(client, ctx, []string{"k1", "k2"})
fmt.Println(val["k1"].ToString()) // this is the k1 value
```

--------------------------------

TITLE: Golang: Verifying Client-Side Cache Hit with IsCacheHit in Rueidis
DESCRIPTION: This Go example demonstrates how to verify if a response was served from the client-side cache using the `IsCacheHit()` method in rueidis. It performs a `GET` operation with caching and then checks the boolean return value of `IsCacheHit()` to determine if it was a cache hit.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
client.DoCache(ctx, client.B().Get().Key("k1").Cache(), time.Minute).IsCacheHit() == true
```

--------------------------------

TITLE: Mocking client.Receive with gomock for Pub/Sub
DESCRIPTION: Illustrates how to mock the `client.Receive` method for testing Pub/Sub functionality using `go.uber.org/mock/gomock`. This example shows how to simulate receiving messages on a channel by providing a callback function that emits a `rueidis.PubSubMessage`. It uses `mock.Match` to identify the SUBSCRIBE command.

SOURCE: https://github.com/redis/rueidis/blob/main/mock/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"
	"github.com/redis/rueidis/mock"
)

func TestWithRueidisReceive(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()
	client := mock.NewClient(ctrl)

	client.EXPECT().Receive(ctx, mock.Match("SUBSCRIBE", "ch"), gomock.Any()).Do(func(_, _ any, fn func(message rueidis.PubSubMessage)) {
		fn(rueidis.PubSubMessage{Message: "msg"})
	})

	client.Receive(ctx, client.B().Subscribe().Channel("ch").Build(), func(msg rueidis.PubSubMessage) {
		if msg.Message != "msg" {
			t.Fatalf("unexpected val %v", msg.Message)
		}
	})
}
```

--------------------------------

TITLE: Benchmark Pipelining with Rueidis in Go
DESCRIPTION: Illustrates how to leverage Rueidis's auto pipelining feature for performance benchmarking. It shows concurrent execution of GET commands within a `b.RunParallel` loop, which are automatically pipelined by the client.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: golang
CODE:
```
func BenchmarkPipelining(b *testing.B, client rueidis.Client) {
	// the below client.Do() operations will be issued from
	// multiple goroutines and thus will be pipelined automatically.
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			client.Do(context.Background(), client.B().Get().Key("k").Build()).ToString()
		}
	})
}
```

--------------------------------

TITLE: Implement CAS Transactions with WATCH, MULTI, EXEC in Rueidis
DESCRIPTION: Provides an example of implementing Compare-And-Swap (CAS) transactions using the WATCH, MULTI, and EXEC commands in Redis via the Rueidis client. This ensures atomic updates by monitoring specified keys and executing a series of commands only if the watched keys have not been modified. It requires a Redis client and a context.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"

	"github.com/redis/rueidis"
)

func main() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()

	// Transfer funds atomically using CAS transaction
	err = client.Dedicated(func(c rueidis.DedicatedClient) error {
		// Watch keys for changes
		c.Do(ctx, c.B().Watch().Key("account:1", "account:2").Build())

		// Read current balances
		balance1, _ := c.Do(ctx, c.B().Get().Key("account:1").Build()).AsInt64()
		balance2, _ := c.Do(ctx, c.B().Get().Key("account:2").Build()).AsInt64()

		// Check if transfer is possible
		transferAmount := int64(100)
		if balance1 < transferAmount {
			return fmt.Errorf("insufficient funds")
		}

		// Execute transaction
		results := c.DoMulti(ctx,
			c.B().Multi().Build(),
			c.B().Decrby().Key("account:1").Decrement(transferAmount).Build(),
			c.B().Incrby().Key("account:2").Increment(transferAmount).Build(),
			c.B().Exec().Build(),
		)

		// Check if transaction succeeded
		exec := results[len(results)-1]
		if err := exec.Error(); err != nil {
			return fmt.Errorf("transaction failed: %w", err)
		}

		arr, _ := exec.AsArray()
		if len(arr) == 0 {
			return fmt.Errorf("transaction aborted (keys were modified)")
		}

		fmt.Println("Transfer successful")
		return nil
	})

	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}

```

--------------------------------

TITLE: Manual Control of Redis Transactions with Dedicate() in Rueidis
DESCRIPTION: Illustrates how to use the client.Dedicate() method to obtain a dedicated client connection for manual control over Redis transactions. This example shows watching a key, reading its value, and performing a conditional restock operation within a MULTI-EXEC block, ensuring atomicity and handling potential transaction aborts.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
ctx := context.Background()

dedicated, cancel := client.Dedicate()
defer cancel()

// Watch keys
dedicated.Do(ctx, dedicated.B().Watch().Key("inventory:item1").Build())

// Read and compute
stock, _ := dedicated.Do(ctx, dedicated.B().Get().Key("inventory:item1").Build()).AsInt64()
if stock < 5 {
	// Restock in transaction
	results := dedicated.DoMulti(ctx,
		dedicated.B().Multi().Build(),
		dedicated.B().Set().Key("inventory:item1").Value("100").Build(),
		dedicated.B().Exec().Build(),
	)

	if exec := results[len(results)-1]; exec.Error() == nil {
		arr, _ := exec.AsArray()
		if len(arr) > 0 {
			fmt.Println("Restock completed")
		}
	}
}

```

--------------------------------

TITLE: Golang: Checking Client-Side Cache TTL with CacheTTL in Rueidis
DESCRIPTION: This Go snippet illustrates how to check the remaining client-side Time-To-Live (TTL) of a cached response in rueidis using the `CacheTTL()` method. It performs a `GET` operation with caching, and then asserts that the returned cache TTL in seconds matches the expected value.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
client.DoCache(ctx, client.B().Get().Key("k1").Cache(), time.Minute).CacheTTL() == 60
```

--------------------------------

TITLE: Handle Mixed JSON Structures in Redis Array Responses in Go
DESCRIPTION: Illustrates a scenario where DecodeSliceOfJSON in Go might fail if the Redis array response contains mixed JSON structures. It shows an example of setting a pure string value and attempting to decode it into a struct slice, leading to an error, and suggests using AsStrSlice() instead for such cases.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
// Set a pure string value
if err = client.Do(ctx, client.B().Set().Key("user1").Value("userName1").Build()).Error(); err != nil {
	return err
}

// Bad
users := make([]*User, 0)
if err := rueidis.DecodeSliceOfJSON(client.Do(ctx, client.B().Mget().Key("user1").Build()), &users); err != nil {
	return err
}
// -> Error: invalid character 'u' looking for the beginning of the value
// in this case, use client.Do(ctx, client.B().Mget().Key("user1").Build()).AsStrSlice()
```

--------------------------------

TITLE: Initialize and Use Rueidis Client in Go
DESCRIPTION: Demonstrates how to create a new Rueidis client instance, connect to a Redis server, and execute basic commands like SET and HGETALL. It includes error handling and resource cleanup using defer.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: golang
CODE:
```
package main

import (
	"context"
	"github.com/redis/rueidis"
)

func main() {
	client, err := rueidis.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()
	// SET key val NX
	err = client.Do(ctx, client.B().Set().Key("key").Value("val").Nx().Build()).Error()
	// HGETALL hm
	hm, err := client.Do(ctx, client.B().Hgetall().Key("hm").Build()).AsStrMap()
}
```

--------------------------------

TITLE: Instantiate Redis Client with Rueidis Go
DESCRIPTION: Demonstrates creating a new Redis client using `rueidis.NewClient` with different connection configurations. Supports single nodes, standalone with replicas, clusters, and sentinels. Error handling is included for client creation.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
// Connect to a single redis node:
client, err := rueidis.NewClient(rueidis.ClientOption{
    InitAddress: []string{"127.0.0.1:6379"},
})

// Connect to a standalone redis with replicas
client, err := rueidis.NewClient(rueidis.ClientOption{
    InitAddress: []string{"127.0.0.1:6379"},
    Standalone: rueidis.StandaloneOption{
        // Note that these addresses must be online and cannot be promoted.
        // An example use case is the reader endpoint provided by cloud vendors.
        ReplicaAddress: []string{"reader_endpoint:port"},
    },
    SendToReplicas: func(cmd rueidis.Completed) bool {
        return cmd.IsReadOnly()
    },
})

// Connect to a redis cluster
client, err := rueidis.NewClient(rueidis.ClientOption{
    InitAddress: []string{"127.0.0.1:7001", "127.0.0.1:7002", "127.0.0.1:7003"},
    ShuffleInit: true,
})

// Connect to a redis cluster and use replicas for read operations
client, err := rueidis.NewClient(rueidis.ClientOption{
    InitAddress: []string{"127.0.0.1:7001", "127.0.0.1:7002", "127.0.0.1:7003"},
    SendToReplicas: func(cmd rueidis.Completed) bool {
        return cmd.IsReadOnly()
    },
})

// Connect to sentinels
client, err := rueidis.NewClient(rueidis.ClientOption{
    InitAddress: []string{"127.0.0.1:26379", "127.0.0.1:26380", "127.0.0.1:26381"},
    Sentinel: rueidis.SentinelOption{
        MasterSet: "my_master",
    },
})
```

--------------------------------

TITLE: Pub/Sub: Pattern and Sharded Subscriptions
DESCRIPTION: Demonstrates advanced Pub/Sub capabilities in Redis using Rueidis, including subscribing to patterns with `Psubscribe` and using sharded Pub/Sub (available in Redis 7.0+) with `Ssubscribe`. Requires a running Redis instance.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
ctx := context.Background()

// Pattern subscription
err := client.Receive(ctx,
	client.B().Psubscribe().Pattern("user:*", "order:*").Build(),
	func(msg rueidis.PubSubMessage) {
		fmt.Printf("Pattern: %s, Channel: %s, Message: %s\n",
			msg.Pattern, msg.Channel, msg.Message)
	})

// Sharded pub/sub (Redis 7.0+)
err = client.Receive(ctx,
	client.B().Ssubscribe().Channel("shard1").Build(),
	func(msg rueidis.PubSubMessage) {
		fmt.Printf("Sharded message: %s\n", msg.Message)
	})
```

--------------------------------

TITLE: Rueidislimiter Initialization and Usage
DESCRIPTION: Demonstrates how to initialize the rate limiter and perform basic rate limiting operations.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidislimiter/README.md

LANGUAGE: APIDOC
CODE:
```
## Initialize Rate Limiter

### Description
Creates a new rate limiter with the specified options.

### Method
`NewRateLimiter`

### Parameters
#### Options (`rueidislimiter.RateLimiterOption`)
- **ClientOption** (`rueidis.ClientOption`) - Required - Options to connect to Redis.
- **KeyPrefix** (`string`) - Required - Prefix for Redis keys used by this limiter.
- **Limit** (`int64`) - Required - Maximum number of allowed requests per window.
- **Window** (`time.Duration`) - Required - Time window duration for rate limiting. Must be greater than 1 millisecond.

### Request Example
```go
limiter, err := rueidislimiter.NewRateLimiter(rueidislimiter.RateLimiterOption{
	ClientOption: rueidis.ClientOption{InitAddress: []string{"localhost:6379"}},
	KeyPrefix:    "api_rate_limit",
	Limit:        5,
	Window:       time.Minute,
})
```

## Check Request

### Description
Checks if a request is allowed under the rate limit without incrementing the count.

### Method
`Check`

### Parameters
#### Path Parameters
- **ctx** (`context.Context`) - Required - The context for the request.
- **identifier** (`string`) - Required - The identifier for the entity being rate limited (e.g., user ID, IP address).

### Response
#### Success Response (200)
- **Result** (`rueidislimiter.Result`) - The result of the check.
  - **Allowed** (`bool`) - Whether the request is allowed.
  - **Remaining** (`int64`) - Number of remaining requests in the current window.
  - **ResetAtMs** (`int64`) - Unix timestamp in milliseconds at which the rate limit will reset.

### Request Example
```go
result, err := limiter.Check(ctx, "user_identifier")
```

## Allow Single Request

### Description
Allows a single request, incrementing the counter if allowed.

### Method
`Allow`

### Parameters
#### Path Parameters
- **ctx** (`context.Context`) - Required - The context for the request.
- **identifier** (`string`) - Required - The identifier for the entity being rate limited.

### Response
#### Success Response (200)
- **Result** (`rueidislimiter.Result`) - The result of allowing the request.
  - **Allowed** (`bool`) - Whether the request was allowed.
  - **Remaining** (`int64`) - Number of remaining requests in the current window.
  - **ResetAtMs** (`int64`) - Unix timestamp in milliseconds at which the rate limit will reset.

### Request Example
```go
result, err := limiter.Allow(ctx, "user_identifier")
```

## Allow Multiple Requests

### Description
Allows `n` requests, incrementing the counter accordingly if allowed.

### Method
`AllowN`

### Parameters
#### Path Parameters
- **ctx** (`context.Context`) - Required - The context for the request.
- **identifier** (`string`) - Required - The identifier for the entity being rate limited.
- **n** (`int64`) - Required - The number of requests to allow.

### Response
#### Success Response (200)
- **Result** (`rueidislimiter.Result`) - The result of allowing the requests.
  - **Allowed** (`bool`) - Whether the requests were allowed.
  - **Remaining** (`int64`) - Number of remaining requests in the current window.
  - **ResetAtMs** (`int64`) - Unix timestamp in milliseconds at which the rate limit will reset.

### Request Example
```go
result, err := limiter.AllowN(ctx, "user_identifier", 3)
```
```

--------------------------------

TITLE: Go Redis Manual Pipelining with DoMulti
DESCRIPTION: Explains how to manually group multiple Redis commands into a single pipeline using `DoMulti`. This is useful for executing a batch of commands atomically and efficiently.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
ctx := context.Background()

// Build multiple commands
cmds := make(rueidis.Commands, 0, 10)
for i := 0; i < 10; i++ {
	key := fmt.Sprintf("counter:%d", i)
	cmds = append(cmds, client.B().Incr().Key(key).Build())
}

// Execute all commands in a single pipeline
results := client.DoMulti(ctx, cmds...)
for i, resp := range results {
	if err := resp.Error(); err != nil {
		fmt.Printf("Command %d failed: %v\n", i, err)
		continue
	}
	count, _ := resp.AsInt64()
	fmt.Printf("Counter %d: %d\n", i, count)
}
```

--------------------------------

TITLE: Pub/Sub: Subscribe and Receive Messages
DESCRIPTION: Illustrates how to subscribe to Redis channels and receive messages using the `Receive` function. It includes handling message callbacks and setting context timeouts for the subscription. Requires a running Redis instance.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/rueidis"
)

func main() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Receive blocks until unsubscribe or context done
	err = client.Receive(ctx,
		client.B().Subscribe().Channel("notifications", "alerts").Build(),
		func(msg rueidis.PubSubMessage) {
			fmt.Printf("Channel: %s, Message: %s\n", msg.Channel, msg.Message)

			// Heavy processing should be in a goroutine
			if msg.Channel == "alerts" {
				go processAlert(msg.Message)
			}
		})

	if err != nil {
		fmt.Printf("Subscription ended: %v\n", err)
	}
}

func processAlert(message string) {
	fmt.Printf("Processing alert: %s\n", message)
}
```

--------------------------------

TITLE: Create Rueidis Client with OpenTelemetry (Golang)
DESCRIPTION: Initializes a Rueidis Redis client with OpenTelemetry tracing and connection metrics enabled. This function requires the `github.com/redis/rueidis` and `github.com/redis/rueidis/rueidisotel` packages. It takes `rueidis.ClientOption` as input and returns a `rueidis.Client` or an error. Note: This feature is not supported on Go 1.18 and 1.19.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidisotel/README.md

LANGUAGE: go
CODE:
```
package main

import (
    "github.com/redis/rueidis"
    "github.com/redis/rueidis/rueidisotel"
)

func main() {
    client, err := rueidisotel.NewClient(rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}})
    if err != nil {
        panic(err)
    }
    defer client.Close()
}
```

--------------------------------

TITLE: Configure Rueidis Client with Memory and Performance Options
DESCRIPTION: This Go code snippet demonstrates how to initialize a Rueidis Redis client with various performance and memory tuning options. It includes settings for connection pooling, buffer sizes, client-side cache, pipelining, connection lifetime, timeouts, and retry mechanisms. These options help optimize the client for high-throughput and low-latency scenarios.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
client, err := rueidis.NewClient(rueidis.ClientOption{
	InitAddress: []string{"127.0.0.1:6379"},

	// Connection pool for blocking commands
	BlockingPoolSize: 1024,

	// Pipeline buffer size (2^10 = 1024 slots per connection)
	RingScaleEachConn: 10,

	// Read/write buffers per connection
	ReadBufferEachConn:  512 * 1024, // 512 KiB
	WriteBufferEachConn: 512 * 1024, // 512 KiB

	// Client-side cache size per connection
	CacheSizeEachConn: 128 * 1024 * 1024, // 128 MiB

	// Pipeline multiplexing (2^2 = 4 connections)
	PipelineMultiplex: 2,

	// Disable auto-pipelining (use connection pool instead)
	DisableAutoPipelining: false,

	// Always pipeline (even for sequential requests)
	AlwaysPipelining: false,

	// Connection lifetime
	ConnLifetime: 30 * time.Minute,

	// Timeouts
	ConnWriteTimeout: 10 * time.Second,
	Dialer: net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 1 * time.Second,
	},

	// Retry configuration
	DisableRetry: false,
	RetryDelay: func(attempts int, cmd rueidis.Completed, err error) time.Duration {
		return time.Duration(attempts) * 100 * time.Millisecond
	},
})

```

--------------------------------

TITLE: Mocking client.Do and client.DoMulti with gomock
DESCRIPTION: Demonstrates how to mock the `client.Do` and `client.DoMulti` methods using `go.uber.org/mock/gomock`. This allows faking Redis responses for individual commands and multiple commands, including handling Redis NIL responses. It utilizes `mock.Match` for command matching and `mock.Result` for faking responses.

SOURCE: https://github.com/redis/rueidis/blob/main/mock/README.md

LANGUAGE: go
CODE:
```
package main

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"
	"github.com/redis/rueidis/mock"
)

func TestWithRueidis(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()
	client := mock.NewClient(ctrl)

	client.EXPECT().Do(ctx, mock.Match("GET", "key")).Return(mock.Result(mock.RedisString("val")))
	if v, _ := client.Do(ctx, client.B().Get().Key("key").Build()).ToString(); v != "val" {
		t.Fatalf("unexpected val %v", v)
	}
	client.EXPECT().DoMulti(ctx, mock.Match("GET", "c"), mock.Match("GET", "d")).Return([]rueidis.RedisResult{
		mock.Result(mock.RedisNil()),
		mock.Result(mock.RedisNil()),
	})
	for _, resp := range client.DoMulti(ctx, client.B().Get().Key("c").Build(), client.B().Get().Key("d").Build()) {
		if err := resp.Error(); !rueidis.IsRedisNil(err) {
			t.Fatalf("unexpected err %v", err)
		}
	}
}
```

--------------------------------

TITLE: Lua Scripting: Batch Execution with ExecMulti
DESCRIPTION: Shows how to execute a single Lua script against multiple keys in a batch operation using `ExecMulti`. This optimizes performance by reducing network round trips. Requires a running Redis instance.

SOURCE: https://github.com/redis/rueidis/blob/main/context7.md

LANGUAGE: go
CODE:
```
ctx := context.Background()

// Script to get and increment counter
script := rueidis.NewLuaScript(`
	local val = redis.call('INCR', KEYS[1])
	return val
`)

// Execute script for multiple keys
multi := []rueidis.LuaExec{
	{Keys: []string{"counter:a"}, Args: nil},
	{Keys: []string{"counter:b"}, Args: nil},
	{Keys: []string{"counter:c"}, Args: nil},
}

// Batch execution with SCRIPT LOAD + EVALSHA
results := script.ExecMulti(ctx, client, multi...)
for i, result := range results {
	count, _ := result.AsInt64()
	fmt.Printf("Counter %d: %d\n", i, count)
}
```

--------------------------------

TITLE: Initialize Rate Limiter with rueidislimiter.NewRateLimiter
DESCRIPTION: This Go code snippet shows how to create a new rate limiter instance. It requires a `rueidislimiter.RateLimiterOption` struct, which includes Redis client options, a key prefix, the request limit, and the time window.

SOURCE: https://github.com/redis/rueidis/blob/main/rueidislimiter/README.md

LANGUAGE: go
CODE:
```
limiter, err := rueidislimiter.NewRateLimiter(rueidislimiter.RateLimiterOption{
	ClientOption: rueidis.ClientOption{InitAddress: []string{"localhost:6379"}},
	KeyPrefix:    "api_rate_limit",
	Limit:        5,
	Window:       time.Second,
})
```

--------------------------------

TITLE: Working with Binary Data and JSON in Rueidis Go
DESCRIPTION: Illustrates how Rueidis handles binary-safe strings, allowing direct storage of `[]byte`. It demonstrates using `rueidis.BinaryString` and the `rueidis.JSON` helper for encoding JSON values, ensuring correct formatting for RedisJSON commands.

SOURCE: https://github.com/redis/rueidis/blob/main/README.md

LANGUAGE: go
CODE:
```
client.B().Set().Key("b").Value(rueidis.BinaryString([]byte{...})).Build()

client.B().JsonSet().Key("j").Path("$.myStrField").Value(rueidis.JSON("str")).Build()
// equivalent to
client.B().JsonSet().Key("j").Path("$.myStrField").Value(`"str"`).Build()
```