package tsdb

import (
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/meta"
)

type Mapper interface {
	// Open will open the necessary resources to being the map job. Could be connections to remote servers or
	// hitting the local store
	Open() error

	// Close will close the mapper
	Close()

	NextChunk() (*rawMapperOutput, error)
}

type Executor interface {
	Execute() <-chan *influxql.Row
}

type Planner struct {
	MetaStore interface {
		ShardGroupsByTimeRange(database, policy string, min, max time.Time) (a []meta.ShardGroupInfo, err error)
		NodeID() uint64
	}

	Cluster interface {
		NewMapper(shardID uint64) (Mapper, error)
	}

	store *Store

	Logger *log.Logger
}

func NewPlanner(store *Store) *Planner {
	return &Planner{
		store:  store,
		Logger: log.New(os.Stderr, "[planner] ", log.LstdFlags),
	}
}

// Plan creates an execution plan for the given SelectStatement and returns an Executor.
func (p *Planner) Plan(stmt *influxql.SelectStatement, chunkSize int) (Executor, error) {
	shards := map[uint64]meta.ShardInfo{} // Shards requiring mappers.

	for _, src := range stmt.Sources {
		mm, ok := src.(*influxql.Measurement)
		if !ok {
			return nil, fmt.Errorf("invalid source type: %#v", src)
		}

		// Replace instances of "now()" with the current time, and check the resultant times.
		stmt.Condition = influxql.Reduce(stmt.Condition, &influxql.NowValuer{Now: time.Now().UTC()})
		tmin, tmax := influxql.TimeRange(stmt.Condition)
		if tmax.IsZero() {
			tmax = time.Now()
		}
		if tmin.IsZero() {
			tmin = time.Unix(0, 0)
		}

		// Build the set of target shards. Using shard IDs as keys ensures each shard ID
		// occurs only once.
		shardGroups, err := p.MetaStore.ShardGroupsByTimeRange(mm.Database, mm.RetentionPolicy, tmin, tmax)
		if err != nil {
			return nil, err
		}
		for _, g := range shardGroups {
			for _, sh := range g.Shards {
				shards[sh.ID] = sh
			}
		}
	}

	// Build the Mappers, one per shard. If the shard is local to this node, always use
	// that one, versus asking the cluster.
	mappers := []Mapper{}
	for _, sh := range shards {
		if sh.OwnedBy(p.MetaStore.NodeID()) {
			shard := p.store.Shard(sh.ID)
			if shard == nil {
				// If the store returns nil, no data has actually been written to the shard.
				// In this case, since there is no data, don't make a mapper.
				continue
			}
			mappers = append(mappers, NewRawMapper(shard, stmt))
		} else {
			mapper, err := p.Cluster.NewMapper(sh.ID)
			if err != nil {
				return nil, err
			}
			if mapper == nil {
				// No error, but there shard doesn't actually exist anywhere on the cluster.
				// This means no data has been written to it, so forget about the mapper.
				continue
			}
			mappers = append(mappers, mapper)
		}
	}

	return NewRawExecutor(mappers, stmt.NamesInSelect()), nil
}

// RawExecutor is an executor for RawMappers.
type RawExecutor struct {
	mappers     []Mapper
	selectNames []string
}

// NewRawExecutor returns a new RawExecutor.
func NewRawExecutor(mappers []Mapper, selectNames []string) *RawExecutor {
	return &RawExecutor{
		mappers:     mappers,
		selectNames: selectNames,
	}
}

// Execute begins execution of the query and returns a channel to receive rows.
func (re *RawExecutor) Execute() <-chan *influxql.Row {
	// Create output channel and stream data in a separate goroutine.
	out := make(chan *influxql.Row, 0)
	go re.execute(out)
	return out
}

func (re *RawExecutor) execute(out chan *influxql.Row) {
	// It's important that all resources are released when execution completes.
	defer re.close()

	// Open the mappers.
	for _, m := range re.mappers {
		if err := m.Open(); err != nil {
			out <- &influxql.Row{Err: err}
			return
		}
	}

	// Drain each mapper, grouping results by tagset.
	mapperOutput := make(map[string]*rawMapperOutput, len(re.mappers))
	for _, m := range re.mappers {
		for {
			chunk, err := m.NextChunk()
			if err != nil {
				out <- &influxql.Row{Err: err}
				return
			}
			if chunk == nil {
				// Mapper has no (more) matching data.
				break
			}

			// Is there an existing mapper-output object for this tagset? If not,
			// use this output. If there is, then append new values to it. This
			// merges data across mappers, for each tagset.
			var output = mapperOutput[chunk.key()]
			if mapperOutput[chunk.key()] == nil {
				output = chunk
			} else {
				output.values = append(output.values, chunk.values...)
			}
		}
	}

	for _, v := range mapperOutput {
		out <- re.processRawResults(v)
	}

	// XXX Results need to be sorted.
	// XXX Limit and chunk checking here.
	// XXX Convert results to "rows"
	close(out)
}

// processRawResults will handle converting the results from a raw query into a Row. It is responsible
// for sorting the results by time.
func (re *RawExecutor) processRawResults(output *rawMapperOutput) *influxql.Row {
	sort.Sort(output.values)
	selectNames := re.selectNames

	// ensure that time is in the select names and in the first position
	hasTime := false
	for i, n := range selectNames {
		if n == "time" {
			// Swap time to the first argument for names
			if i != 0 {
				selectNames[0], selectNames[i] = selectNames[i], selectNames[0]
			}
			hasTime = true
			break
		}
	}

	// time should always be in the list of names they get back
	if !hasTime {
		selectNames = append([]string{"time"}, selectNames...)
	}

	// since selectNames can contain tags, we need to strip them out
	selectFields := make([]string, 0, len(selectNames))

	for _, n := range selectNames {
		if _, found := output.tags[n]; !found {
			selectFields = append(selectFields, n)
		}
	}

	row := &influxql.Row{
		Name:    output.measurement,
		Tags:    output.tags,
		Columns: selectFields,
	}

	// if they've selected only a single value we have to handle things a little differently
	singleValue := len(selectFields) == influxql.SelectColumnCountWithOneValue

	// the results will have all of the raw mapper results, convert into the row
	for _, v := range output.values {
		vals := make([]interface{}, len(selectFields))

		if singleValue {
			vals[0] = time.Unix(0, v.time).UTC()
			vals[1] = v.value.(interface{})
		} else {
			fields := v.value.(map[string]interface{})

			// time is always the first value
			vals[0] = time.Unix(0, v.time).UTC()

			// populate the other values
			for i := 1; i < len(selectFields); i++ {
				vals[i] = fields[selectFields[i]]
			}
		}

		row.Values = append(row.Values, vals)
	}

	return row
}

// Close closes the executor such that all resources are released. Once closed,
// an executor may not be re-used.
func (re *RawExecutor) close() {
	for _, m := range re.mappers {
		m.Close()
	}
}