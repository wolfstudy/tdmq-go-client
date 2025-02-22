// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/gogo/protobuf/proto"

	"github.com/TencentCloud/tdmq-go-client/pulsar/log"

	"github.com/TencentCloud/tdmq-go-client/pulsar/internal"
	"github.com/TencentCloud/tdmq-go-client/pulsar/internal/compression"
	pb "github.com/TencentCloud/tdmq-go-client/pulsar/internal/pulsar_proto"
)

const (
	// producer states
	producerInit int32 = iota
	producerReady
	producerClosing
	producerClosed
)

var (
	errFailAddBatch    = errors.New("message send failed")
	errSendTimeout     = errors.New("message send timeout")
	errSendQueueIsFull = errors.New("producer send queue is full")
	errMessageTooLarge = errors.New("message size exceeds MaxMessageSize")

	buffersPool sync.Pool
)

// metric error types
const (
	publishErrorTimeout     = "timeout"
	publishErrorMsgTooLarge = "msg_too_large"
)

var (
	messagesPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pulsar_client_messages_published",
		Help: "Counter of messages published by the client",
	})

	bytesPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pulsar_client_bytes_published",
		Help: "Counter of messages published by the client",
	})

	messagesPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pulsar_client_producer_pending_messages",
		Help: "Counter of messages pending to be published by the client",
	})

	bytesPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pulsar_client_producer_pending_bytes",
		Help: "Counter of bytes pending to be published by the client",
	})

	publishErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pulsar_client_producer_errors",
		Help: "Counter of publish errors",
	}, []string{"error"})

	publishLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pulsar_client_producer_latency_seconds",
		Help:    "Publish latency experienced by the client",
		Buckets: []float64{.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})

	publishRPCLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pulsar_client_producer_rpc_latency_seconds",
		Help:    "Publish RPC latency experienced internally by the client when sending data to receiving an ack",
		Buckets: []float64{.0005, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})
)

type partitionProducer struct {
	state  int32
	client *client
	topic  string
	log    log.Logger
	cnx    internal.Connection

	options             *ProducerOptions
	producerName        string
	producerID          uint64
	batchBuilder        internal.BatchBuilder
	sequenceIDGenerator *uint64
	batchFlushTicker    *time.Ticker

	// Channel where app is posting messages to be published
	eventsChan      chan interface{}
	connectClosedCh chan connectionClosed

	publishSemaphore internal.Semaphore
	pendingQueue     internal.BlockingQueue
	lastSequenceID   int64
	schemaInfo       *SchemaInfo
	partitionIdx     int32
}

func newPartitionProducer(client *client, topic string, options *ProducerOptions, partitionIdx int) (
	*partitionProducer, error) {
	var batchingMaxPublishDelay time.Duration
	if options.BatchingMaxPublishDelay != 0 {
		batchingMaxPublishDelay = options.BatchingMaxPublishDelay
	} else {
		batchingMaxPublishDelay = defaultBatchingMaxPublishDelay
	}

	var maxPendingMessages int
	if options.MaxPendingMessages == 0 {
		maxPendingMessages = 1000
	} else {
		maxPendingMessages = options.MaxPendingMessages
	}

	logger := client.log.SubLogger(log.Fields{"topic": topic})

	p := &partitionProducer{
		state:            producerInit,
		client:           client,
		topic:            topic,
		log:              logger,
		options:          options,
		producerID:       client.rpcClient.NewProducerID(),
		eventsChan:       make(chan interface{}, maxPendingMessages),
		connectClosedCh:  make(chan connectionClosed, 10),
		batchFlushTicker: time.NewTicker(batchingMaxPublishDelay),
		publishSemaphore: internal.NewSemaphore(int32(maxPendingMessages)),
		pendingQueue:     internal.NewBlockingQueue(maxPendingMessages),
		lastSequenceID:   -1,
		partitionIdx:     int32(partitionIdx),
	}

	if options.Schema != nil && options.Schema.GetSchemaInfo() != nil {
		p.schemaInfo = options.Schema.GetSchemaInfo()
	} else {
		p.schemaInfo = nil
	}

	if options.Name != "" {
		p.producerName = options.Name
	}

	err := p.grabCnx()
	if err != nil {
		logger.WithError(err).Error("Failed to create producer")
		return nil, err
	}

	p.log = p.log.SubLogger(log.Fields{
		"producer_name": p.producerName,
		"producerID":    p.producerID,
	})

	p.log.WithField("cnx", p.cnx.ID()).Info("Created producer")
	atomic.StoreInt32(&p.state, producerReady)

	go p.runEventsLoop()

	return p, nil
}

func (p *partitionProducer) grabCnx() error {
	lr, err := p.client.lookupService.Lookup(p.topic)
	if err != nil {
		p.log.WithError(err).Warn("Failed to lookup topic")
		return err
	}

	p.log.Debug("Lookup result: ", lr)
	id := p.client.rpcClient.NewRequestID()

	// set schema info for producer

	pbSchema := new(pb.Schema)
	if p.schemaInfo != nil {
		tmpSchemaType := pb.Schema_Type(int32(p.schemaInfo.Type))
		pbSchema = &pb.Schema{
			Name:       proto.String(p.schemaInfo.Name),
			Type:       &tmpSchemaType,
			SchemaData: []byte(p.schemaInfo.Schema),
			Properties: internal.ConvertFromStringMap(p.schemaInfo.Properties),
		}
		p.log.Debugf("The partition consumer schema name is: %s", pbSchema.Name)
	} else {
		pbSchema = nil
		p.log.Debug("The partition consumer schema is nil")
	}

	cmdProducer := &pb.CommandProducer{
		RequestId:  proto.Uint64(id),
		Topic:      proto.String(p.topic),
		Encrypted:  nil,
		ProducerId: proto.Uint64(p.producerID),
		Schema:     pbSchema,
	}

	if p.producerName != "" {
		cmdProducer.ProducerName = proto.String(p.producerName)
	}

	//for Tencent cloud cam
	if len(p.options.Properties) > 0 && p.client.options.AuthCloud == nil {
		cmdProducer.Metadata = toKeyValues(p.options.Properties)
	} else if p.client.options.AuthCloud != nil {
		metaMap := make(map[string]string)
		if len(p.options.Properties) > 0 {
			// putAll properties
			for k, v := range p.options.Properties {
				metaMap[k] = v
			}
		}
		metaMap["topic"] = p.topic[strings.Index(p.topic, "//")+2:]
		metaMap["clientId"] = strconv.FormatUint(p.producerID, 10)
		metaMap["requestId"] = strconv.FormatUint(id, 10)
		p.client.options.AuthCloud.CreateAuthMetadata("producerMessage", metaMap)
		cmdProducer.Metadata = toKeyValues(metaMap)

		metadataBytes, _ := json.Marshal(metaMap)
		p.log.Info(string(metadataBytes))
	}

	res, err := p.client.rpcClient.Request(lr.LogicalAddr, lr.PhysicalAddr, id, pb.BaseCommand_PRODUCER, cmdProducer)
	if err != nil {
		p.log.WithError(err).Error("Failed to create producer")
		return err
	}

	p.producerName = res.Response.ProducerSuccess.GetProducerName()
	if p.options.DisableBatching {
		provider, _ := GetBatcherBuilderProvider(DefaultBatchBuilder)
		p.batchBuilder, err = provider(p.options.BatchingMaxMessages, p.options.BatchingMaxSize,
			p.producerName, p.producerID, pb.CompressionType(p.options.CompressionType),
			compression.Level(p.options.CompressionLevel),
			p,
			p.log)
		if err != nil {
			return err
		}
	} else if p.batchBuilder == nil {
		provider, err := GetBatcherBuilderProvider(p.options.BatcherBuilderType)
		if err != nil {
			provider, _ = GetBatcherBuilderProvider(DefaultBatchBuilder)
		}

		p.batchBuilder, err = provider(p.options.BatchingMaxMessages, p.options.BatchingMaxSize,
			p.producerName, p.producerID, pb.CompressionType(p.options.CompressionType),
			compression.Level(p.options.CompressionLevel),
			p,
			p.log)
		if err != nil {
			return err
		}
	}

	if p.sequenceIDGenerator == nil {
		nextSequenceID := uint64(res.Response.ProducerSuccess.GetLastSequenceId() + 1)
		p.sequenceIDGenerator = &nextSequenceID
	}
	p.cnx = res.Cnx
	p.cnx.RegisterListener(p.producerID, p)
	p.log.WithField("cnx", res.Cnx.ID()).Debug("Connected producer")

	pendingItems := p.pendingQueue.ReadableSlice()
	if len(pendingItems) > 0 {
		p.log.Infof("Resending %d pending batches", len(pendingItems))
		for _, pi := range pendingItems {
			p.cnx.WriteData(pi.(*pendingItem).batchData)
		}
	}
	return nil
}

type connectionClosed struct{}

func (p *partitionProducer) GetBuffer() internal.Buffer {
	b, ok := buffersPool.Get().(internal.Buffer)
	if ok {
		b.Clear()
	}
	return b
}

func (p *partitionProducer) ConnectionClosed() {
	// Trigger reconnection in the produce goroutine
	p.log.WithField("cnx", p.cnx.ID()).Warn("Connection was closed")
	p.connectClosedCh <- connectionClosed{}
}

func (p *partitionProducer) reconnectToBroker() {
	var (
		maxRetry int
		backoff  = internal.Backoff{}
	)

	if p.options.MaxReconnectToBroker == nil {
		maxRetry = -1
	} else {
		maxRetry = int(*p.options.MaxReconnectToBroker)
	}

	for maxRetry != 0 {
		if atomic.LoadInt32(&p.state) != producerReady {
			// Producer is already closing
			return
		}

		d := backoff.Next()
		p.log.Info("Reconnecting to broker in ", d)
		time.Sleep(d)

		err := p.grabCnx()
		if err == nil {
			// Successfully reconnected
			p.log.WithField("cnx", p.cnx.ID()).Info("Reconnected producer to broker")
			return
		}

		if maxRetry > 0 {
			maxRetry--
		}
	}
}

func (p *partitionProducer) runEventsLoop() {
	for {
		select {
		case i := <-p.eventsChan:
			switch v := i.(type) {
			case *sendRequest:
				p.internalSend(v)
			case *flushRequest:
				p.internalFlush(v)
			case *closeProducer:
				p.internalClose(v)
				return
			}
		case <-p.connectClosedCh:
			p.reconnectToBroker()
		case <-p.batchFlushTicker.C:
			if p.batchBuilder.IsMultiBatches() {
				p.internalFlushCurrentBatches()
			} else {
				p.internalFlushCurrentBatch()
			}
		}
	}
}

func (p *partitionProducer) Topic() string {
	return p.topic
}

func (p *partitionProducer) Name() string {
	return p.producerName
}

func (p *partitionProducer) internalSend(request *sendRequest) {
	p.log.Debug("Received send request: ", *request)

	msg := request.msg

	payload := msg.Payload
	var schemaPayload []byte
	var err error
	if p.options.Schema != nil {
		schemaPayload, err = p.options.Schema.Encode(msg.Value)
		if err != nil {
			return
		}
	}

	if payload == nil {
		payload = schemaPayload
	}

	// if msg is too large
	if len(payload) > int(p.cnx.GetMaxMessageSize()) {
		p.publishSemaphore.Release()
		request.callback(nil, request.msg, errMessageTooLarge)
		p.log.WithError(errMessageTooLarge).
			WithField("size", len(payload)).
			WithField("properties", msg.Properties).
			Error()
		publishErrors.WithLabelValues(publishErrorMsgTooLarge).Inc()
		return
	}

	deliverAt := msg.DeliverAt
	if msg.DeliverAfter.Nanoseconds() > 0 {
		deliverAt = time.Now().Add(msg.DeliverAfter)
	}

	sendAsBatch := !p.options.DisableBatching &&
		msg.ReplicationClusters == nil &&
		deliverAt.UnixNano() < 0

	smm := &pb.SingleMessageMetadata{
		PayloadSize: proto.Int(len(payload)),
	}

	if msg.EventTime.UnixNano() != 0 {
		smm.EventTime = proto.Uint64(internal.TimestampMillis(msg.EventTime))
	}

	if msg.Key != "" {
		smm.PartitionKey = proto.String(msg.Key)
	}

	if msg.Properties != nil {
		smm.Properties = internal.ConvertFromStringMap(msg.Properties)
	}

	//For Tencent TDMQ tag message
	var tagStr string
	for idx, tag := range msg.Tags {
		if len(tag) == 0 {
			continue
		}
		if idx == len(msg.Tags)-1 {
			tagStr += strings.TrimSpace(tag)
		} else {
			tagStr += strings.TrimSpace(tag) + "||"
		}
	}
	if len(tagStr) > 0 {
		if msg.Properties == nil {
			msg.Properties = make(map[string]string)
		}
		sendAsBatch = false
		p.options.DisableBatching = true

		tagKeyValue := &pb.KeyValue{
			Key:   proto.String("TAGS"),
			Value: proto.String(tagStr),
		}
		smm.Properties = append([]*pb.KeyValue{tagKeyValue}, smm.Properties...)
	}

	if msg.SequenceID != nil {
		sequenceID := uint64(*msg.SequenceID)
		smm.SequenceId = proto.Uint64(sequenceID)
	}

	if !sendAsBatch {
		p.internalFlushCurrentBatch()
	}
	added := p.batchBuilder.Add(smm, p.sequenceIDGenerator, payload, request,
		msg.ReplicationClusters, deliverAt)
	if !added {
		// The current batch is full.. flush it and retry
		p.internalFlushCurrentBatch()

		// after flushing try again to add the current payload
		if ok := p.batchBuilder.Add(smm, p.sequenceIDGenerator, payload, request,
			msg.ReplicationClusters, deliverAt); !ok {
			p.publishSemaphore.Release()
			request.callback(nil, request.msg, errFailAddBatch)
			p.log.WithField("size", len(payload)).
				WithField("properties", msg.Properties).
				Error("unable to add message to batch")
			return
		}
	}

	if !sendAsBatch || request.flushImmediately {
		if p.batchBuilder.IsMultiBatches() {
			p.internalFlushCurrentBatches()
		} else {
			p.internalFlushCurrentBatch()
		}
	}
}

type pendingItem struct {
	sync.Mutex
	batchData    internal.Buffer
	sequenceID   uint64
	sentAt       time.Time
	sendRequests []interface{}
	completed    bool
}

func (p *partitionProducer) internalFlushCurrentBatch() {
	if p.options.SendTimeout > 0 {
		p.failTimeoutMessages()
	}

	batchData, sequenceID, callbacks := p.batchBuilder.Flush()
	if batchData == nil {
		return
	}

	p.pendingQueue.Put(&pendingItem{
		sentAt:       time.Now(),
		batchData:    batchData,
		sequenceID:   sequenceID,
		sendRequests: callbacks,
	})
	p.cnx.WriteData(batchData)
}

func (p *partitionProducer) failTimeoutMessages() {
	// since Closing/Closed connection couldn't be reopen, load and compare is safe
	state := atomic.LoadInt32(&p.state)
	if state == producerClosing || state == producerClosed {
		return
	}

	item := p.pendingQueue.Peek()
	if item == nil {
		// pending queue is empty
		return
	}

	pi := item.(*pendingItem)
	if time.Since(pi.sentAt) < p.options.SendTimeout {
		// pending messages not timeout yet
		return
	}

	p.log.Infof("Failing %d messages", p.pendingQueue.Size())
	for p.pendingQueue.Size() > 0 {
		pi = p.pendingQueue.Poll().(*pendingItem)
		pi.Lock()
		for _, i := range pi.sendRequests {
			sr := i.(*sendRequest)
			if sr.msg != nil {
				size := len(sr.msg.Payload)
				p.publishSemaphore.Release()
				messagesPending.Dec()
				bytesPending.Sub(float64(size))
				publishErrors.WithLabelValues(publishErrorTimeout).Inc()
				p.log.WithError(errSendTimeout).
					WithField("size", size).
					WithField("properties", sr.msg.Properties)
			}
			if sr.callback != nil {
				sr.callback(nil, sr.msg, errSendTimeout)
			}
		}
		buffersPool.Put(pi.batchData)
		pi.Unlock()
	}
}

func (p *partitionProducer) internalFlushCurrentBatches() {
	batchesData, sequenceIDs, callbacks := p.batchBuilder.FlushBatches()
	if batchesData == nil {
		return
	}

	for i := range batchesData {
		if batchesData[i] == nil {
			continue
		}
		p.pendingQueue.Put(&pendingItem{
			batchData:    batchesData[i],
			sequenceID:   sequenceIDs[i],
			sendRequests: callbacks[i],
		})
		p.cnx.WriteData(batchesData[i])
	}

}

func (p *partitionProducer) internalFlush(fr *flushRequest) {
	if p.batchBuilder.IsMultiBatches() {
		p.internalFlushCurrentBatches()
	} else {
		p.internalFlushCurrentBatch()
	}

	pi, ok := p.pendingQueue.PeekLast().(*pendingItem)
	if !ok {
		fr.waitGroup.Done()
		return
	}

	// lock the pending request while adding requests
	// since the ReceivedSendReceipt func iterates over this list
	pi.Lock()
	defer pi.Unlock()

	if pi.completed {
		// The last item in the queue has been completed while we were
		// looking at it. It's safe at this point to assume that every
		// message enqueued before Flush() was called are now persisted
		fr.waitGroup.Done()
		return
	}

	sendReq := &sendRequest{
		msg: nil,
		callback: func(id MessageID, message *ProducerMessage, e error) {
			fr.err = e
			fr.waitGroup.Done()
		},
	}

	pi.sendRequests = append(pi.sendRequests, sendReq)
}

func (p *partitionProducer) Send(ctx context.Context, msg *ProducerMessage) (MessageID, error) {
	wg := sync.WaitGroup{}
	wg.Add(1)

	var err error
	var msgID MessageID

	p.internalSendAsync(ctx, msg, func(ID MessageID, message *ProducerMessage, e error) {
		err = e
		msgID = ID
		wg.Done()
	}, true)

	wg.Wait()
	return msgID, err
}

func (p *partitionProducer) SendAsync(ctx context.Context, msg *ProducerMessage,
	callback func(MessageID, *ProducerMessage, error)) {
	p.internalSendAsync(ctx, msg, callback, false)
}

func (p *partitionProducer) internalSendAsync(ctx context.Context, msg *ProducerMessage,
	callback func(MessageID, *ProducerMessage, error), flushImmediately bool) {
	sr := &sendRequest{
		ctx:              ctx,
		msg:              msg,
		callback:         callback,
		flushImmediately: flushImmediately,
		publishTime:      time.Now(),
	}
	p.options.Interceptors.BeforeSend(p, msg)

	if p.options.DisableBlockIfQueueFull {
		if !p.publishSemaphore.TryAcquire() {
			if callback != nil {
				callback(nil, msg, errSendQueueIsFull)
			}
			return
		}
	} else {
		p.publishSemaphore.Acquire()
	}

	messagesPending.Inc()
	bytesPending.Add(float64(len(sr.msg.Payload)))

	p.eventsChan <- sr
}

func (p *partitionProducer) ReceivedSendReceipt(response *pb.CommandSendReceipt) {
	pi, ok := p.pendingQueue.Peek().(*pendingItem)

	if !ok {
		// if we receive a receipt although the pending queue is empty, the state of the broker and the producer differs.
		// At that point, it is better to close the connection to the broker to reconnect to a broker hopping it solves
		// the state discrepancy.
		p.log.Warnf("Received ack for %v although the pending queue is empty, closing connection", response.GetMessageId())
		p.cnx.Close()
		return
	}

	if pi.sequenceID != response.GetSequenceId() {
		// if we receive a receipt that is not the one expected, the state of the broker and the producer differs.
		// At that point, it is better to close the connection to the broker to reconnect to a broker hopping it solves
		// the state discrepancy.
		p.log.Warnf("Received ack for %v on sequenceId %v - expected: %v, closing connection", response.GetMessageId(),
			response.GetSequenceId(), pi.sequenceID)
		p.cnx.Close()
		return
	}

	// The ack was indeed for the expected item in the queue, we can remove it and trigger the callback
	p.pendingQueue.Poll()

	now := time.Now().UnixNano()

	// lock the pending item while sending the requests
	pi.Lock()
	defer pi.Unlock()
	publishRPCLatency.Observe(float64(now-pi.sentAt.UnixNano()) / 1.0e9)
	for idx, i := range pi.sendRequests {
		sr := i.(*sendRequest)
		if sr.msg != nil {
			atomic.StoreInt64(&p.lastSequenceID, int64(pi.sequenceID))
			p.publishSemaphore.Release()

			publishLatency.Observe(float64(now-sr.publishTime.UnixNano()) / 1.0e9)
			messagesPublished.Inc()
			messagesPending.Dec()
			payloadSize := float64(len(sr.msg.Payload))
			bytesPublished.Add(payloadSize)
			bytesPending.Sub(payloadSize)
		}

		if sr.callback != nil || len(p.options.Interceptors) > 0 {
			msgID := newMessageID(
				int64(response.MessageId.GetLedgerId()),
				int64(response.MessageId.GetEntryId()),
				int32(idx),
				p.partitionIdx,
			)

			if sr.callback != nil {
				sr.callback(msgID, sr.msg, nil)
			}

			p.options.Interceptors.OnSendAcknowledgement(p, sr.msg, msgID)
		}
	}

	// Mark this pending item as done
	pi.completed = true
	// Return buffer to the pool since we're now done using it
	buffersPool.Put(pi.batchData)
}

func (p *partitionProducer) internalClose(req *closeProducer) {
	defer req.waitGroup.Done()
	if !atomic.CompareAndSwapInt32(&p.state, producerReady, producerClosing) {
		return
	}

	p.log.Info("Closing producer")

	id := p.client.rpcClient.NewRequestID()
	_, err := p.client.rpcClient.RequestOnCnx(p.cnx, id, pb.BaseCommand_CLOSE_PRODUCER, &pb.CommandCloseProducer{
		ProducerId: &p.producerID,
		RequestId:  &id,
	})

	if err != nil {
		p.log.WithError(err).Warn("Failed to close producer")
	} else {
		p.log.Info("Closed producer")
	}

	if err = p.batchBuilder.Close(); err != nil {
		p.log.WithError(err).Warn("Failed to close batch builder")
	}

	atomic.StoreInt32(&p.state, producerClosed)
	p.cnx.UnregisterListener(p.producerID)
	p.batchFlushTicker.Stop()
}

func (p *partitionProducer) LastSequenceID() int64 {
	return atomic.LoadInt64(&p.lastSequenceID)
}

func (p *partitionProducer) Flush() error {
	wg := sync.WaitGroup{}
	wg.Add(1)

	cp := &flushRequest{&wg, nil}
	p.eventsChan <- cp

	wg.Wait()
	return cp.err
}

func (p *partitionProducer) Close() {
	if atomic.LoadInt32(&p.state) != producerReady {
		// Producer is closing
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(1)

	cp := &closeProducer{&wg}
	p.eventsChan <- cp

	wg.Wait()
}

type sendRequest struct {
	ctx              context.Context
	msg              *ProducerMessage
	callback         func(MessageID, *ProducerMessage, error)
	publishTime      time.Time
	flushImmediately bool
}

type closeProducer struct {
	waitGroup *sync.WaitGroup
}

type flushRequest struct {
	waitGroup *sync.WaitGroup
	err       error
}
