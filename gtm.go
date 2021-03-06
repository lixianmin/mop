package mop

import (
	"context"
	"fmt"
	"github.com/lixianmin/logo"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

var opCodes = [...]string{"c", "i", "u", "d"}

type OrderingGuarantee int

type errchk interface {
	Err() error
}

type task struct {
	doneC  chan bool
	stopC  chan bool
	ctx    context.Context
	cancel context.CancelFunc
}

func newTask(stopC chan bool) *task {
	ctx, cancel := context.WithCancel(context.Background())
	t := &task{
		stopC:  stopC,
		doneC:  make(chan bool),
		ctx:    ctx,
		cancel: cancel,
	}
	go t.start()
	return t
}

func (t *task) start() {
	defer t.cancel()
	select {
	case <-t.doneC:
		break
	case <-t.stopC:
		break
	}
}

func (t *task) Done() {
	close(t.doneC)
}

const (
	Oplog     OrderingGuarantee = iota // ops sent in oplog order (strong ordering)
	Namespace                          // ops sent in oplog order within a namespace
	Document                           // ops sent in oplog order for a single document
	AnyOrder                           // ops sent as they become available
)

type QuerySource int

const (
	OplogQuerySource QuerySource = iota
	DirectQuerySource
)

type Optime struct {
	Timestamp primitive.Timestamp "ts"
}

type ReplStatus struct {
	Optimes map[string]*Optime "optimes"
}

type OpLog struct {
	Timestamp    primitive.Timestamp    "ts"
	HistoryID    int64                  "h"
	MongoVersion int                    "v"
	Operation    string                 "op"
	Namespace    string                 "ns"
	Doc          map[string]interface{} "o"
	Update       map[string]interface{} "o2"
}

type ChangeDocNs struct {
	Database   string "db"
	Collection string "coll"
}

type Doc struct {
	Id interface{} "_id"
}

type CollectionStats struct {
	Count         int32 "count"
	AvgObjectSize int32 "avgObjSize"
}

type OpChan chan *Op

type OpLogEntry map[string]interface{}

type OpFilter func(*Op) bool

type ShardInsertHandler func(*ShardInfo) (*mongo.Client, error)

type TimestampGenerator func(*mongo.Client, *Options) (primitive.Timestamp, error)

type DataUnmarshaller func(namespace string, data []byte) (interface{}, error)

type PipelineBuilder func(namespace string, changeStream bool) ([]interface{}, error)

type OpBuf struct {
	Entries        []*Op
	BufferSize     int
	BufferDuration time.Duration
}

type OpCtx struct {
	lock             *sync.Mutex
	OpC              OpChan
	ErrC             chan error
	DirectReadWg     *sync.WaitGroup
	directReadConcWg *sync.WaitGroup
	stopC            chan bool
	allWg            *sync.WaitGroup
	seekC            chan primitive.Timestamp
	pauseC           chan bool
	resumeC          chan bool
	paused           bool
	stopped          bool
	log              logo.ILogger
}

type OpCtxMulti struct {
	lock         *sync.Mutex
	contexts     []*OpCtx
	OpC          OpChan
	ErrC         chan error
	DirectReadWg *sync.WaitGroup
	opWg         *sync.WaitGroup
	stopC        chan bool
	allWg        *sync.WaitGroup
	seekC        chan primitive.Timestamp
	pauseC       chan bool
	resumeC      chan bool
	paused       bool
	stopped      bool
	log          logo.ILogger
}

type ShardInfo struct {
	hostname string
}

type N struct {
	database   string
	collection string
}

func (n *N) parse(ns string) (err error) {
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("Invalid ns: %s :expecting db.collection", ns)
	} else {
		n.database = parts[0]
		n.collection = parts[1]
	}
	return
}

func (n *N) parseForChanges(ns string) {
	if ns == "" {
		// watch the whole deployment
		n.database = ""
		n.collection = ""
		return
	}
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) == 1 {
		n.database = parts[0]
		n.collection = ""
	} else {
		n.database = parts[0]
		n.collection = parts[1]
	}
	return
}

func (n *N) desc() (dsc string) {
	if n.isDatabase() {
		dsc = fmt.Sprintf("database %s", n.database)
	} else if n.isCollection() {
		dsc = fmt.Sprintf("collection %s.%s", n.database, n.collection)
	} else {
		dsc = "the deployment"
	}
	return
}

func (n *N) isDeployment() bool {
	return n.database == "" && n.collection == ""
}

func (n *N) isDatabase() bool {
	return n.database != "" && n.collection == ""
}

func (n *N) isCollection() bool {
	return n.database != "" && n.collection != ""
}

func (shard *ShardInfo) GetURL() string {
	hostParts := strings.SplitN(shard.hostname, "/", 2)
	if len(hostParts) == 2 {
		return "mongodb://" + hostParts[1] + "?replicaSet=" + hostParts[0]
	} else {
		return "mongodb://" + hostParts[0]
	}
}

func (ctx *OpCtx) isStopped() bool {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	return ctx.stopped
}

func (ctx *OpCtx) Since(ts primitive.Timestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	ctx.seekC <- ts
}

func (ctx *OpCtx) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
	}
}

func (ctx *OpCtx) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
	}
}

func (ctx *OpCtx) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		ctx.allWg.Wait()
		close(ctx.OpC)
		close(ctx.ErrC)
	}
}

func (ctx *OpCtxMulti) Since(ts primitive.Timestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	for _, child := range ctx.contexts {
		child.Since(ts)
	}
}

func (ctx *OpCtxMulti) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
		for _, child := range ctx.contexts {
			child.Pause()
		}
	}
}

func (ctx *OpCtxMulti) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
		for _, child := range ctx.contexts {
			child.Resume()
		}
	}
}

func (ctx *OpCtxMulti) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		for _, c := range ctx.contexts {
			child := c
			go child.Stop()
		}
		ctx.allWg.Wait()
		ctx.opWg.Wait()
		close(ctx.OpC)
		close(ctx.ErrC)
	}
}

func unwrapErr(err error) error {
	if err == nil {
		return nil
	}
	// go.mongodb.org/mongo-driver/x/network/connection 这个代码路径没有了
	//if ce, ok := err.(connection.Error); ok {
	//	if ce.Wrapped != nil {
	//		return unwrapErr(ce.Wrapped)
	//	}
	//}
	return err
}

func positionLost(ec errchk) bool {
	err := unwrapErr(ec.Err())
	if err != nil {
		if ce, ok := err.(mongo.CommandError); ok {
			code := ce.Code
			if code == 136 { // cursor capped position lost
				return true
			}
		}
	}
	return false
}

func cursorTimeout(ec errchk) bool {
	err := unwrapErr(ec.Err())
	if et, ok := err.(net.Error); ok {
		return et.Timeout() || et.Temporary()
	}
	return false
}

func invalidCursor(ec errchk) bool {
	err := unwrapErr(ec.Err())
	if err != nil {
		if ce, ok := err.(mongo.CommandError); ok {
			code := ce.Code
			if code == 43 { // cursor not found
				return true
			}
			if code == 11601 { // cursor interrupted
				return true
			}
			if code == 136 { // cursor capped position lost
				return true
			}
			if code == 237 { // cursor killed
				return true
			}
		}
	}
	return false
}

func tailShards(multi *OpCtxMulti, ctx *OpCtx, o *Options, handler ShardInsertHandler) {
	defer multi.allWg.Done()
	if o == nil {
		o = DefaultOptions()
	} else {
		o.SetDefaults()
	}
	for {
		select {
		case <-multi.stopC:
			return
		case <-multi.pauseC:
			select {
			case <-multi.stopC:
				return
			case <-multi.resumeC:
				break
			}
		case err := <-ctx.ErrC:
			if err == nil {
				break
			}
			multi.ErrC <- err
		case op := <-ctx.OpC:
			if op == nil || op.Data == nil || op.Data["host"] == nil {
				break
			}
			shardHost, ok := op.Data["host"].(string)
			if !ok {
				break
			}
			// new shard detected
			multi.lock.Lock()
			if multi.stopped {
				multi.lock.Unlock()
				break
			}
			shardInfo := &ShardInfo{
				hostname: shardHost,
			}
			shardClient, err := handler(shardInfo)
			if err != nil {
				multi.ErrC <- errors.Wrap(err, "Error calling shard handler")
				multi.lock.Unlock()
				break
			}
			shardCtx := Start(shardClient, o)
			multi.contexts = append(multi.contexts, shardCtx)
			multi.DirectReadWg.Add(1)
			multi.allWg.Add(1)
			multi.opWg.Add(2)
			go func() {
				defer multi.DirectReadWg.Done()
				shardCtx.DirectReadWg.Wait()
			}()
			go func() {
				defer multi.allWg.Done()
				shardCtx.allWg.Wait()
			}()
			go func(c OpChan) {
				defer multi.opWg.Done()
				for op := range c {
					multi.OpC <- op
				}
			}(shardCtx.OpC)
			go func(c chan error) {
				defer multi.opWg.Done()
				for err := range c {
					multi.ErrC <- err
				}
			}(shardCtx.ErrC)
			multi.lock.Unlock()
		}
	}
}

func (ctx *OpCtxMulti) AddShardListener(
	configSession *mongo.Client, shardOptions *Options, handler ShardInsertHandler) {
	opts := DefaultOptions()
	opts.NamespaceFilter = func(op *Op) bool {
		return op.Namespace == "config.shards" && op.IsInsert()
	}
	configCtx := Start(configSession, opts)
	ctx.allWg.Add(1)
	go tailShards(ctx, configCtx, shardOptions, handler)
}

func ChainOpFilters(filters ...OpFilter) OpFilter {
	return func(op *Op) bool {
		for _, filter := range filters {
			if filter(op) == false {
				return false
			}
		}
		return true
	}
}

func (this *OpBuf) Append(op *Op) {
	this.Entries = append(this.Entries, op)
}

func (this *OpBuf) IsFull() bool {
	return len(this.Entries) >= this.BufferSize
}

func (this *OpBuf) HasOne() bool {
	return len(this.Entries) == 1
}

func (this *OpBuf) Flush(client *mongo.Client, ctx *OpCtx, o *Options) {
	if len(this.Entries) == 0 {
		return
	}
	ns := make(map[string][]interface{})
	byId := make(map[interface{}][]*Op)
	for _, op := range this.Entries {
		if op.IsUpdate() && op.Doc == nil {
			idKey := fmt.Sprintf("%s.%v", op.Namespace, op.Id)
			ns[op.Namespace] = append(ns[op.Namespace], op.Id)
			byId[idKey] = append(byId[idKey], op)
		}
	}
retry:
	for n, opIds := range ns {
		var parts = strings.SplitN(n, ".", 2)
		db, col := parts[0], parts[1]
		sel := bson.M{"_id": bson.M{"$in": opIds}}
		collection := client.Database(db).Collection(col)
		cursor, err := collection.Find(context.Background(), sel)
		if err == nil {
			for cursor.Next(context.Background()) {
				doc := make(map[string]interface{})
				if err = cursor.Decode(&doc); err == nil {
					resultId := fmt.Sprintf("%s.%v", n, doc["_id"])
					if ops, ok := byId[resultId]; ok {
						for _, op := range ops {
							op.processData(doc, o)
						}
					}

				}
			}
			if err = cursor.Close(context.Background()); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error finding documents to associate with ops")
			}
		} else {
			ctx.ErrC <- errors.Wrap(err, "Error finding documents to associate with ops")
			break retry
		}
	}
	for _, op := range this.Entries {
		if op.matchesFilter(o) {
			ctx.OpC <- op
		}
	}
	this.Entries = nil
}

func UpdateIsReplace(entry map[string]interface{}) bool {
	if _, ok := entry["$set"]; ok {
		return false
	} else if _, ok := entry["$unset"]; ok {
		return false
	} else {
		return true
	}
}

func (my *Op) shouldParse() bool {
	return my.IsInsert() || my.IsDelete() || my.IsUpdate() || my.IsCommand()
}

func (my *Op) matchesNsFilter(o *Options) bool {
	return o.NamespaceFilter == nil || o.NamespaceFilter(my)
}

func (my *Op) matchesFilter(o *Options) bool {
	return o.Filter == nil || o.Filter(my)
}

func (my *Op) matchesDirectFilter(o *Options) bool {
	return o.DirectReadFilter == nil || o.DirectReadFilter(my)
}

func normalizeDocSlice(a []interface{}) []interface{} {
	var avs []interface{}
	for _, av := range a {
		var avc interface{}
		switch achild := av.(type) {
		case map[string]interface{}:
			avc = normalizeDocMap(achild)
		case primitive.M:
			avc = normalizeDocMap(map[string]interface{}(achild))
		case primitive.D:
			avc = normalizeDocMap(map[string]interface{}(achild.Map()))
		case []interface{}:
			avc = normalizeDocSlice(achild)
		case primitive.A:
			avc = normalizeDocSlice([]interface{}(achild))
		default:
			avc = av
		}
		avs = append(avs, avc)
	}
	return avs
}

func normalizeDocMap(m map[string]interface{}) map[string]interface{} {
	o := map[string]interface{}{}
	for k, v := range m {
		switch child := v.(type) {
		case map[string]interface{}:
			o[k] = normalizeDocMap(child)
		case primitive.M:
			o[k] = normalizeDocMap(map[string]interface{}(child))
		case primitive.D:
			o[k] = normalizeDocMap(map[string]interface{}(child.Map()))
		case []interface{}:
			o[k] = normalizeDocSlice(child)
		case primitive.A:
			o[k] = normalizeDocSlice([]interface{}(child))
		default:
			o[k] = v
		}
	}
	return o
}

func (my *Op) processData(data interface{}, o *Options) {
	if data != nil {
		my.Doc = data
		if m, ok := data.(map[string]interface{}); ok {
			my.Data = normalizeDocMap(m)
			my.Doc = my.Data
		}
		if o.Unmarshal != nil {
			my.processDoc(data, o)
		}
	}
}

func (my *Op) processDoc(data interface{}, o *Options) {
	if o.Unmarshal == nil || data == nil {
		return
	}
	b, err := bson.Marshal(data)
	if err == nil {
		my.Doc, err = o.Unmarshal(my.Namespace, b)
		if err != nil {
			o.Log.Info("Unable to process document: %s", err)
		}
	} else {
		o.Log.Info("Unable to process document: %s", err)
	}
	return
}

func (my *Op) ParseLogEntry(entry *OpLog, o *Options) (include bool, err error) {
	var rawField map[string]interface{}
	my.Operation = entry.Operation
	my.Timestamp = entry.Timestamp
	my.Namespace = entry.Namespace
	if my.shouldParse() {
		if my.IsCommand() {
			rawField = entry.Doc
			my.processData(rawField, o)
		}
		if my.matchesNsFilter(o) {
			if my.IsInsert() || my.IsDelete() || my.IsUpdate() {
				if my.IsUpdate() {
					rawField = entry.Update
				} else {
					rawField = entry.Doc
				}
				my.Id = rawField["_id"]
				if my.IsInsert() {
					my.processData(rawField, o)
				} else if my.IsUpdate() {
					rawField = entry.Doc
					if o.UpdateDataAsDelta || UpdateIsReplace(rawField) {
						my.processData(rawField, o)
					}
				}
				include = true
			} else if my.IsCommand() {
				include = my.IsDrop()
			}
		}
	}
	return
}

func OpLogCollectionName(client *mongo.Client, o *Options) string {
	return "oplog.rs"
}

func OpLogCollection(client *mongo.Client, o *Options) *mongo.Collection {
	localDB := client.Database(o.OpLogDatabaseName)
	return localDB.Collection(o.OpLogCollectionName)
}

func ParseTimestamp(timestamp primitive.Timestamp) (uint32, uint32) {
	return timestamp.T, timestamp.I
}

func validOps() bson.M {
	return bson.M{"op": bson.M{"$in": opCodes}}
}

func LastOpTimestamp(client *mongo.Client, o *Options) (primitive.Timestamp, error) {
	opLog := OpLog{}
	filter := validOps()
	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"$natural": -1})
	c := OpLogCollection(client, o)
	err := c.FindOne(context.Background(), filter, opts).Decode(&opLog)
	return opLog.Timestamp, err
}

func FirstOpTimestamp(client *mongo.Client, o *Options) (primitive.Timestamp, error) {
	opLog := OpLog{}
	filter := validOps()
	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"$natural": 1})
	c := OpLogCollection(client, o)
	err := c.FindOne(context.Background(), filter, opts).Decode(&opLog)
	return opLog.Timestamp, err
}

func GetOpLogCursor(client *mongo.Client, after primitive.Timestamp, o *Options) (*mongo.Cursor, error) {
	query := bson.M{
		"ts":          bson.M{"$gt": after},
		"op":          bson.M{"$in": opCodes},
		"fromMigrate": bson.M{"$exists": false},
	}
	opts := &options.FindOptions{}
	opts.SetSort(bson.M{"$natural": 1})
	opts.SetCursorType(options.TailableAwait)
	//opts.SetOplogReplay(true)
	//opts.SetNoCursorTimeout(true)
	collection := OpLogCollection(client, o)
	return collection.Find(context.Background(), query, opts)
}

func opDataReady(op *Op, o *Options) (ready bool) {
	if o.UpdateDataAsDelta {
		ready = true
	} else if o.Ordering == AnyOrder {
		if op.IsUpdate() {
			ready = op.Data != nil || op.Doc != nil
		} else {
			ready = true
		}
	}
	return
}

func TailOps(ctx *OpCtx, client *mongo.Client, channels []OpChan, o *Options) error {
	defer ctx.allWg.Done()
	var cursor *mongo.Cursor
	var err error
	var cts primitive.Timestamp
	if o.After != nil {
		cts, _ = o.After(client, o)
	} else {
		cts, _ = LastOpTimestamp(client, o)
	}
	task := newTask(ctx.stopC)
	defer task.Done()
	for task.ctx.Err() == nil {
		cursor, err = GetOpLogCursor(client, cts, o)
		if err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error establishing the oplog cursor")
			continue
		}
		next := true
		for next && cursor.Next(task.ctx) {
			var entry OpLog
			if err = cursor.Decode(&entry); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error decoding the oplog document")
				break
			}
			op := &Op{
				Id:        "",
				Operation: "",
				Namespace: "",
				Data:      nil,
				Timestamp: primitive.Timestamp{},
				Source:    OplogQuerySource,
			}
			ok, err := op.ParseLogEntry(&entry, o)
			if err == nil {
				if ok && op.matchesFilter(o) {
					if opDataReady(op, o) {
						ctx.OpC <- op
					} else {
						// broadcast to fetch channels
						for _, channel := range channels {
							channel <- op
						}
					}
				}
			} else {
				ctx.ErrC <- errors.Wrap(err, "Error parsing the oplog document")
			}
			select {
			case ts := <-ctx.seekC:
				cts = ts
				next = false
			case <-ctx.pauseC:
				cursor.Close(context.Background())
				next = false
				<-ctx.resumeC
				select {
				case ts := <-ctx.seekC:
					cts = ts
				default:
					break
				}
			default:
				cts = op.Timestamp
			}
		}
		if positionLost(cursor) {
			cts, _ = FirstOpTimestamp(client, o)
		}
		cursor.Close(context.Background())
	}
	return nil
}

func DirectReadSegment(ctx *OpCtx, client *mongo.Client, ns string, o *Options, seg *CollectionSegment, stats *CollectionStats) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	defer ctx.directReadConcWg.Done()
	task := newTask(ctx.stopC)
	defer task.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error starting direct reads. Invalid namespace.")
		return
	}
	var batch int32 = 500
	if stats.AvgObjectSize != 0 {
		batch = (2 * 1024 * 1024) / stats.AvgObjectSize // 2MB divided by avg doc size
		if batch < 500 {
			batch = 500
		}
	}
	c := client.Database(n.database).Collection(n.collection)
	sel := seg.toSelector()
	var cursor *mongo.Cursor
	if o.Pipe != nil {
		var pipeline []interface{}
		if pipeline, err = o.Pipe(ns, false); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error building aggregation pipeline stages.")
			return
		}
		if pipeline != nil && len(pipeline) > 0 {
			var stages []interface{}
			stages = append(stages, bson.M{"$match": sel})
			for _, stage := range pipeline {
				stages = append(stages, stage)
			}
			opts := options.Aggregate()
			if batch != 0 {
				opts.SetBatchSize(batch)
			}
			if o.PipeAllowDisk {
				opts.SetAllowDiskUse(true)
			}
			cursor, err = c.Aggregate(task.ctx, stages, opts)
		} else {
			opts := options.Find()
			if o.DirectReadNoTimeout {
				opts.SetNoCursorTimeout(true)
			}
			if batch != 0 {
				opts.SetBatchSize(batch)
			}
			cursor, err = c.Find(task.ctx, sel, opts)
		}
	} else {
		opts := options.Find()
		if o.DirectReadNoTimeout {
			opts.SetNoCursorTimeout(true)
		}
		if batch != 0 {
			opts.SetBatchSize(batch)
		}
		cursor, err = c.Find(task.ctx, sel, opts)
	}
	if err != nil {
		ctx.ErrC <- errors.Wrap(err, fmt.Sprintf("Error performing direct read of collection %s", ns))
		return
	}
	result := map[string]interface{}{}
	for cursor.Next(task.ctx) {
		if err = cursor.Decode(&result); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error decoding cursor in direct reads")
			result = map[string]interface{}{}
			continue
		}
		t := time.Now().UTC().Unix()
		op := &Op{
			Id:        result["_id"],
			Operation: "i",
			Namespace: ns,
			Source:    DirectQuerySource,
			Timestamp: primitive.Timestamp{T: uint32(t)},
		}
		op.processData(result, o)
		if op.matchesDirectFilter(o) {
			ctx.OpC <- op
		}
		result = map[string]interface{}{}
	}
	if err = cursor.Err(); err != nil {
		ctx.ErrC <- errors.Wrap(err, fmt.Sprintf("Error performing direct read of collection %s", ns))
	}
	cursor.Close(context.Background())
	return
}

func GetCollectionStats(ctx *OpCtx, client *mongo.Client, ns string) (stats *CollectionStats, err error) {
	stats = &CollectionStats{}
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error reading collection stats. Invalid namespace.")
		return
	}
	var result *mongo.SingleResult
	cmd := bson.M{"collStats": n.collection}
	result = client.Database(n.database).RunCommand(context.Background(), cmd)
	err = result.Err()
	if err == nil {
		err = result.Decode(stats)
	}
	if stats.Count == 0 {
		var count int64
		col := client.Database(n.database).Collection(n.collection)
		count, err = col.EstimatedDocumentCount(context.Background())
		if err == nil {
			stats.Count = int32(count)
		}
	}
	return
}

func ProcessDirectReads(ctx *OpCtx, client *mongo.Client, o *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	concur := o.DirectReadConcur
	running := 0
	for _, ns := range o.DirectReadNs {
		if concur > 0 && running >= concur {
			ctx.directReadConcWg.Wait()
			running = 0
		}
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		ctx.allWg.Add(1)
		go DirectReadPaged(ctx, client, ns, o)
		running = running + 1
	}
	return
}

func ConsumeChangeStream(ctx *OpCtx, client *mongo.Client, ns string, o *Options) (err error) {
	defer ctx.allWg.Done()
	n := &N{}
	n.parseForChanges(ns)
	ctx.log.Info("Watching changes on %s", n.desc())
	var pipeline []interface{}
	var startAt *primitive.Timestamp
	var resumeAfter interface{}
	if o.After != nil {
		if pos, err := o.After(client, o); err == nil {
			if pos.T > 0 {
				startAt = &pos
			} else if pos.T == 0 {
				if pos, err = FirstOpTimestamp(client, o); err == nil {
					startAt = &pos
				}
			}
		}
	}
	if o.Pipe != nil {
		var stages []interface{}
		if stages, err = o.Pipe(ns, true); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error building aggregation pipeline stages.")
			return
		}
		if stages != nil && len(stages) > 0 {
			pipeline = stages
		}
	}
	task := newTask(ctx.stopC)
	defer task.Done()
	for task.ctx.Err() == nil {
		var stream *mongo.ChangeStream
		opts := options.ChangeStream()
		opts.SetBatchSize(int32(o.ChannelSize))
		opts.SetFullDocument(options.UpdateLookup)
		opts.SetStartAtOperationTime(startAt)
		opts.SetResumeAfter(resumeAfter)
		if n.isDeployment() {
			stream, err = client.Watch(task.ctx, pipeline, opts)
		} else if n.isDatabase() {
			d := client.Database(n.database)
			stream, err = d.Watch(task.ctx, pipeline, opts)
		} else {
			c := client.Database(n.database).Collection(n.collection)
			stream, err = c.Watch(task.ctx, pipeline, opts)
		}
		if err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error starting change stream. Will retry.")
			if stream != nil {
				stream.Close(context.Background())
			}
			continue
		}
		next := true
		for next && stream.Next(task.ctx) {
			var changeDoc ChangeDoc
			if err = stream.Decode(&changeDoc); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error decoding change doc.")
				break
			}
			resumeAfter = changeDoc.Id
			startAt = nil
			oper := changeDoc.mapOperation()
			if changeDoc.isDrop() {
				op := &Op{
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				op.Data = map[string]interface{}{"drop": changeDoc.Namespace.Collection}
				if op.matchesNsFilter(o) {
					ctx.OpC <- op
				}
			} else if changeDoc.isDropDatabase() {
				op := &Op{
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				op.Data = map[string]interface{}{"dropDatabase": changeDoc.Namespace.Database}
				if op.matchesNsFilter(o) {
					ctx.OpC <- op
				}
			} else if changeDoc.isInvalidate() {
				resumeAfter = nil
				startAt = nil
				next = false
				time.Sleep(time.Duration(5) * time.Second)
			} else if oper != "" {
				op := &Op{
					Id:        changeDoc.docId(),
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				if op.matchesNsFilter(o) {
					if changeDoc.hasUpdate() {
						op.UpdateDescription = changeDoc.UpdateDescription
					}
					if changeDoc.hasDoc() {
						op.processData(changeDoc.FullDoc, o)
						if op.matchesDirectFilter(o) {
							ctx.OpC <- op
						}
					} else if op.matchesDirectFilter(o) {
						ctx.OpC <- op
					}
				}
			}
			select {
			case ts := <-ctx.seekC:
				resumeAfter = nil
				startAt = &ts
				next = false
			case <-ctx.pauseC:
				stream.Close(context.Background())
				next = false
				<-ctx.resumeC
				select {
				case ts := <-ctx.seekC:
					resumeAfter = nil
					startAt = &ts
				default:
					break
				}
			default:
				break
			}
		}
		if positionLost(stream) {
			resumeAfter = nil
			startAt = nil
		}
		stream.Close(context.Background())
	}
	return nil
}

func DirectReadPaged(ctx *OpCtx, client *mongo.Client, ns string, o *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	defer ctx.directReadConcWg.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error starting direct reads. Invalid namespace.")
		return
	}
	var stats *CollectionStats
	stats, _ = GetCollectionStats(ctx, client, ns)
	c := client.Database(n.database).Collection(n.collection)
	var maxSplits int32 = o.DirectReadSplitMax
	segment := &CollectionSegment{
		splitKey: "_id",
	}
	if maxSplits <= 0 {
		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		go DirectReadSegment(ctx, client, ns, o, segment, stats)
		return
	}
	const defaultSegmentSize = 50000
	const minSegmentSize = 5000
	var segmentSize int32 = defaultSegmentSize

	if stats.Count != 0 {
		segmentSize = stats.Count / (maxSplits + 1)
		if segmentSize < minSegmentSize {
			ctx.allWg.Add(1)
			ctx.DirectReadWg.Add(1)
			ctx.directReadConcWg.Add(1)
			go DirectReadSegment(ctx, client, ns, o, segment, stats)
			return
		}
	}
	var splitCount int32

	done := false

	task := newTask(ctx.stopC)
	defer task.Done()

	for !done {

		sel := bson.M{}

		if segment.min != nil {
			sel["_id"] = bson.M{"$gte": segment.min}
		}

		stages := []bson.M{
			{"$match": sel},
			{"$sort": bson.M{"_id": 1}},
			{"$skip": segmentSize},
			{"$limit": 1},
			{"$project": bson.M{"_id": 1}},
		}

		opts := options.Aggregate()
		opts.SetAllowDiskUse(o.PipeAllowDisk)

		hasMax := false
		var cursor *mongo.Cursor
		cursor, err = c.Aggregate(task.ctx, stages, opts)
		if err == nil {
			if cursor.Next(task.ctx) {
				var doc Doc
				cursor.Decode(&doc)
				if doc.Id != nil {
					segment.max = doc.Id
					hasMax = true
				}
			}
			cursor.Close(task.ctx)
		}
		done = !hasMax

		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		go DirectReadSegment(ctx, client, ns, o, segment, stats)

		if !done {
			segment = &CollectionSegment{
				splitKey: "_id",
				min:      segment.max,
			}
			splitCount = splitCount + 1
			if splitCount == maxSplits {
				done = true
				ctx.allWg.Add(1)
				ctx.DirectReadWg.Add(1)
				ctx.directReadConcWg.Add(1)
				go DirectReadSegment(ctx, client, ns, o, segment, stats)
			}
		}
	}
	return
}

func FetchDocuments(ctx *OpCtx, client *mongo.Client, filter OpFilter, buf *OpBuf, inOp OpChan, o *Options) error {
	defer ctx.allWg.Done()
	timer := time.NewTimer(buf.BufferDuration)
	timer.Stop()
	for {
		select {
		case <-ctx.stopC:
			return nil
		case <-timer.C:
			buf.Flush(client, ctx, o)
		case op := <-inOp:
			if op == nil {
				break
			}
			if filter(op) {
				buf.Append(op)
				if buf.IsFull() {
					timer.Stop()
					buf.Flush(client, ctx, o)
				} else if buf.HasOne() {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(buf.BufferDuration)
				}
			}
		}
	}
	return nil
}

func OpFilterForOrdering(ordering OrderingGuarantee, workers []string, worker string) OpFilter {
	switch ordering {
	case AnyOrder, Document:
		ring := New(workers)
		return func(op *Op) bool {
			var key string
			if op.Id != nil {
				key = fmt.Sprintf("%v", op.Id)
			} else {
				key = op.Namespace
			}
			if who, ok := ring.GetNode(key); ok {
				return who == worker
			} else {
				return false
			}
		}
	case Namespace:
		ring := New(workers)
		return func(op *Op) bool {
			if who, ok := ring.GetNode(op.Namespace); ok {
				return who == worker
			} else {
				return false
			}
		}
	default:
		return func(op *Op) bool {
			return true
		}
	}
}

func DefaultOptions() *Options {
	return &Options{
		After:               LastOpTimestamp,
		Filter:              nil,
		NamespaceFilter:     nil,
		OpLogDatabaseName:   "local",
		OpLogCollectionName: "oplog.rs",
		ChannelSize:         2048,
		BufferSize:          50,
		BufferDuration:      time.Duration(75) * time.Millisecond,
		Ordering:            Oplog,
		WorkerCount:         10,
		MaxWaitSecs:         10,
		UpdateDataAsDelta:   false,
		DirectReadNs:        []string{},
		DirectReadFilter:    nil,
		DirectReadSplitMax:  9,
		DirectReadConcur:    0,
		DirectReadNoTimeout: false,
		Unmarshal:           nil,
		Log:                 logo.GetLogger(),
	}
}

func defaultUnmarshaller(namespace string, cursor mongo.Cursor) (interface{}, error) {
	var m map[string]interface{}
	if err := cursor.Decode(&m); err == nil {
		return m, nil
	} else {
		return nil, err
	}
}

func (this *Options) SetDefaults() {
	defaultOpts := DefaultOptions()
	if this.ChannelSize < 1 {
		this.ChannelSize = defaultOpts.ChannelSize
	}
	if this.BufferSize < 1 {
		this.BufferSize = defaultOpts.BufferSize
	}
	if this.BufferDuration == 0 {
		this.BufferDuration = defaultOpts.BufferDuration
	}
	if this.Ordering == Oplog {
		this.WorkerCount = 1
	}
	if this.WorkerCount < 1 {
		this.WorkerCount = 1
	}
	if this.UpdateDataAsDelta {
		this.Ordering = Oplog
		this.WorkerCount = 0
	}
	if this.Unmarshal == nil {
		this.Unmarshal = defaultOpts.Unmarshal
	}
	if this.Log == nil {
		this.Log = defaultOpts.Log
	}
	if this.DirectReadConcur == 0 {
		this.DirectReadConcur = defaultOpts.DirectReadConcur
	}
	if this.DirectReadSplitMax == 0 {
		this.DirectReadSplitMax = defaultOpts.DirectReadSplitMax
	}
	if len(this.ChangeStreamNs) == 0 {
		if this.After == nil {
			this.After = defaultOpts.After
		}
	} else {
		this.OpLogDisabled = true
	}
	if this.OpLogDatabaseName == "" {
		this.OpLogDatabaseName = defaultOpts.OpLogDatabaseName
	}
	if this.OpLogCollectionName == "" {
		this.OpLogCollectionName = defaultOpts.OpLogCollectionName
	}
	if this.MaxWaitSecs == 0 {
		this.MaxWaitSecs = defaultOpts.MaxWaitSecs
	}
}

func Tail(client *mongo.Client, o *Options) (OpChan, chan error) {
	ctx := Start(client, o)
	return ctx.OpC, ctx.ErrC
}

func GetShards(client *mongo.Client) (shardInfos []*ShardInfo) {
	// use this for sharded databases to get the shard hosts
	// use the hostnames to create multiple clients for a call to StartMulti
	col := client.Database("config").Collection("shards")
	opts := &options.FindOptions{}
	cursor, err := col.Find(context.Background(), nil, opts)
	if err != nil {
		return
	}
	for cursor.Next(context.Background()) {
		shard := map[string]interface{}{}
		if err = cursor.Decode(&shard); err != nil {
			continue
		}
		if shard["host"] == nil {
			continue
		}
		shardInfo := &ShardInfo{
			hostname: shard["host"].(string),
		}
		shardInfos = append(shardInfos, shardInfo)
	}
	return
}

func StartMulti(clients []*mongo.Client, o *Options) *OpCtxMulti {
	if o == nil {
		o = DefaultOptions()
	} else {
		o.SetDefaults()
	}

	stopC := make(chan bool, 1)
	errC := make(chan error, o.ChannelSize)
	opC := make(OpChan, o.ChannelSize)

	var directReadWg sync.WaitGroup
	var opWg sync.WaitGroup
	var allWg sync.WaitGroup
	var seekC = make(chan primitive.Timestamp, 1)
	var pauseC = make(chan bool, 1)
	var resumeC = make(chan bool, 1)

	ctxMulti := &OpCtxMulti{
		lock:         &sync.Mutex{},
		OpC:          opC,
		ErrC:         errC,
		DirectReadWg: &directReadWg,
		opWg:         &opWg,
		stopC:        stopC,
		allWg:        &allWg,
		pauseC:       pauseC,
		resumeC:      resumeC,
		seekC:        seekC,
		log:          o.Log,
	}

	ctxMulti.lock.Lock()
	defer ctxMulti.lock.Unlock()

	for _, client := range clients {
		ctx := Start(client, o)
		ctxMulti.contexts = append(ctxMulti.contexts, ctx)
		allWg.Add(1)
		directReadWg.Add(1)
		opWg.Add(2)
		go func() {
			defer directReadWg.Done()
			ctx.DirectReadWg.Wait()
		}()
		go func() {
			defer allWg.Done()
			ctx.allWg.Wait()
		}()
		go func(c OpChan) {
			defer opWg.Done()
			for op := range c {
				opC <- op
			}
		}(ctx.OpC)
		go func(c chan error) {
			defer opWg.Done()
			for err := range c {
				errC <- err
			}
		}(ctx.ErrC)
	}
	return ctxMulti
}

func Start(client *mongo.Client, o *Options) *OpCtx {
	if o == nil {
		o = DefaultOptions()
	} else {
		o.SetDefaults()
	}

	stopC := make(chan bool)
	errC := make(chan error, o.ChannelSize)
	opC := make(OpChan, o.ChannelSize)

	var inOps []OpChan
	var workerNames []string
	var directReadWg sync.WaitGroup
	var directReadConcWg sync.WaitGroup
	var allWg sync.WaitGroup

	streams := len(o.ChangeStreamNs)
	if o.OpLogDisabled == false {
		streams += 1
	}

	var seekC = make(chan primitive.Timestamp, streams)
	var pauseC = make(chan bool, streams)
	var resumeC = make(chan bool, streams)

	ctx := &OpCtx{
		lock:             &sync.Mutex{},
		OpC:              opC,
		ErrC:             errC,
		DirectReadWg:     &directReadWg,
		directReadConcWg: &directReadConcWg,
		stopC:            stopC,
		allWg:            &allWg,
		pauseC:           pauseC,
		resumeC:          resumeC,
		seekC:            seekC,
		log:              o.Log,
	}

	if o.OpLogDisabled == false {
		for i := 1; i <= o.WorkerCount; i++ {
			workerNames = append(workerNames, strconv.Itoa(i))
		}
		for i := 1; i <= o.WorkerCount; i++ {
			allWg.Add(1)
			inOp := make(OpChan, o.ChannelSize)
			inOps = append(inOps, inOp)
			buf := &OpBuf{
				BufferSize:     o.BufferSize,
				BufferDuration: o.BufferDuration,
			}
			worker := strconv.Itoa(i)
			filter := OpFilterForOrdering(o.Ordering, workerNames, worker)
			go FetchDocuments(ctx, client, filter, buf, inOp, o)
		}
	}

	if len(o.DirectReadNs) > 0 {
		directReadWg.Add(1)
		allWg.Add(1)
		go ProcessDirectReads(ctx, client, o)
	}

	for _, ns := range o.ChangeStreamNs {
		allWg.Add(1)
		go ConsumeChangeStream(ctx, client, ns, o)
	}

	if o.OpLogDisabled == false {
		allWg.Add(1)
		go TailOps(ctx, client, inOps, o)
	}

	return ctx
}

func (rs *ReplStatus) GetLastCommitted() (ts primitive.Timestamp, err error) {
	if rs.Optimes != nil {
		ot := rs.Optimes["lastCommittedOpTime"]
		if ot != nil && ot.Timestamp.T > 0 {
			ts = ot.Timestamp
			return
		}
	}
	err = fmt.Errorf("lastCommittedOpTime not found")
	return
}

func GetReplStatus(client *mongo.Client) (rs *ReplStatus, err error) {
	db := client.Database("admin")
	result := db.RunCommand(context.Background(), bson.M{
		"replSetGetStatus": 1,
	})
	if err = result.Err(); err == nil {
		rs = &ReplStatus{}
		err = result.Decode(rs)
	}
	return
}
