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
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/golang/protobuf/proto"

	log "github.com/sirupsen/logrus"

	"github.com/TencentCloud/tdmq-go-client/pulsar/internal"
	"github.com/TencentCloud/tdmq-go-client/pulsar/internal/auth"
	"github.com/TencentCloud/tdmq-go-client/pulsar/internal/pb"
)

const (
	defaultConnectionTimeout = 30 * time.Second
	defaultOperationTimeout  = 30 * time.Second
)

const (
	NetmodelIntranetPulsar = "INTRANET_PULSAR"

	NetmodelIntranetPulsarSsl = "INTRANET_PULSAR_SSL"

	NetmodelExtranetPulsar = "EXTRANET_PULSAR"

	NetmodelExtenetPulsarSsl = "EXTRANET_PULSAR_SSL"

	NetmodelVpcPulsar = "VPC_PULSAR"

	NetmodelVpcPulsarSsl = "VPC_PULSAR_SSL"
)

type client struct {
	// For Tencent TDMQ
	options *ClientOptions
	//original
	cnxPool       internal.ConnectionPool
	rpcClient     internal.RPCClient
	handlers      internal.ClientHandlers
	lookupService internal.LookupService
}

func newClient(options ClientOptions) (Client, error) {
	if options.URL == "" {
		return nil, newError(ResultInvalidConfiguration, "URL is required for client")
	}

	url, err := url.Parse(options.URL)
	if err != nil {
		log.WithError(err).Error("Failed to parse service URL")
		return nil, newError(ResultInvalidConfiguration, "Invalid service URL")
	}

	var tlsConfig *internal.TLSOptions
	switch url.Scheme {
	case "pulsar":
		tlsConfig = nil
	case "pulsar+ssl":
		tlsConfig = &internal.TLSOptions{
			AllowInsecureConnection: options.TLSAllowInsecureConnection,
			TrustCertsFilePath:      options.TLSTrustCertsFilePath,
			ValidateHostname:        options.TLSValidateHostname,
		}
	default:
		return nil, newError(ResultInvalidConfiguration, fmt.Sprintf("Invalid URL scheme '%s'", url.Scheme))
	}

	var authProvider auth.Provider
	var ok bool

	if options.Authentication == nil {
		authProvider = auth.NewAuthDisabled()
	} else {
		authProvider, ok = options.Authentication.(auth.Provider)
		if !ok {
			return nil, errors.New("invalid auth provider interface")
		}
	}
	err = authProvider.Init()
	if err != nil {
		return nil, err
	}

	connectionTimeout := options.ConnectionTimeout
	if connectionTimeout.Nanoseconds() == 0 {
		connectionTimeout = defaultConnectionTimeout
	}

	operationTimeout := options.OperationTimeout
	if operationTimeout.Nanoseconds() == 0 {
		operationTimeout = defaultOperationTimeout
	}


	if options.NetModel == "" {
		options.NetModel = NetmodelVpcPulsar
	}

	maxConnectionsPerHost := options.MaxConnectionsPerBroker
	if maxConnectionsPerHost <= 0 {
		maxConnectionsPerHost = 1
	}

	c := &client{
		options: &options,
		cnxPool: internal.NewConnectionPoolWithAuthCloud(options.AuthCloud, tlsConfig, authProvider, connectionTimeout,maxConnectionsPerHost),
	}
	c.rpcClient = internal.NewRPCClient(url, c.cnxPool, operationTimeout)
	c.lookupService = internal.NewLookupService(c.rpcClient, url, tlsConfig != nil)
	c.handlers = internal.NewClientHandlers()
	return c, nil
}

func (c *client) CreateProducer(options ProducerOptions) (Producer, error) {
	producer, err := newProducer(c, &options)
	if err == nil {
		c.handlers.Add(producer)
	}
	return producer, err
}

func (c *client) Subscribe(options ConsumerOptions) (Consumer, error) {
	consumer, err := newConsumer(c, options)
	if err != nil {
		return nil, err
	}
	c.handlers.Add(consumer)
	return consumer, nil
}

func (c *client) CreateReader(options ReaderOptions) (Reader, error) {
	reader, err := newReader(c, options)
	if err != nil {
		return nil, err
	}
	c.handlers.Add(reader)
	return reader, nil
}

func (c *client) TopicPartitions(topic string) ([]string, error) {
	topicName, err := internal.ParseTopicName(topic)
	if err != nil {
		return nil, err
	}

	id := c.rpcClient.NewRequestID()
	res, err := c.rpcClient.RequestToAnyBroker(id, pb.BaseCommand_PARTITIONED_METADATA,
		&pb.CommandPartitionedTopicMetadata{
			RequestId: &id,
			Topic:     &topicName.Name,
			NetModel:  &c.options.NetModel,
		})
	if err != nil {
		return nil, err
	}

	r := res.Response.PartitionMetadataResponse
	if r != nil {
		if r.Error != nil {
			return nil, newError(ResultLookupError, r.GetError().String())
		}

		if r.GetPartitions() > 0 {
			partitions := make([]string, r.GetPartitions())
			for i := 0; i < int(r.GetPartitions()); i++ {
				partitions[i] = fmt.Sprintf("%s-partition-%d", topic, i)
			}
			return partitions, nil
		}
	}

	// Non-partitioned topic
	return []string{topicName.Name}, nil
}

func (c *client) Close() {
	c.handlers.Close()
}

func (c *client) namespaceTopics(namespace string) ([]string, error) {
	id := c.rpcClient.NewRequestID()
	req := &pb.CommandGetTopicsOfNamespace{
		RequestId: proto.Uint64(id),
		Namespace: proto.String(namespace),
		Mode:      pb.CommandGetTopicsOfNamespace_PERSISTENT.Enum(),
		NetModel:  proto.String(c.options.NetModel),
	}
	res, err := c.rpcClient.RequestToAnyBroker(id, pb.BaseCommand_GET_TOPICS_OF_NAMESPACE, req)
	if err != nil {
		return nil, err
	}
	if res.Response.Error != nil {
		return []string{}, newError(ResultLookupError, res.Response.GetError().String())
	}

	return res.Response.GetTopicsOfNamespaceResponse.GetTopics(), nil
}
