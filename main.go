// Copyright (c) 2021 Doc.ai its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/debug"
	"github.com/edwarnicke/grpcfd"
	"github.com/edwarnicke/signalctx"
	"github.com/edwarnicke/vpphelper"
	"github.com/google/uuid"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/connectioncontext"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/mechanisms/memif"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/up"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"
	"github.com/networkservicemesh/sdk/pkg/networkservice/utils/metadata"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/logger"
	"github.com/networkservicemesh/sdk/pkg/tools/logger/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/nsurl"
	"github.com/networkservicemesh/sdk/pkg/tools/opentracing"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
)

// Config - configuration for cmd-forwarder-vpp
type Config struct {
	Name             string        `default:"cmd-nsc-vpp" desc:"Name of Endpoint"`
	DialTimeout      time.Duration `default:"5s" desc:"timeout to dial NSMgr" split_words:"true"`
	RequestTimeout   time.Duration `default:"15s" desc:"timeout to request NSE" split_words:"true"`
	ConnectTo        url.URL       `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime time.Duration `default:"24h" desc:"maximum lifetime of tokens" split_words:"true"`
	NetworkServices  []url.URL     `default:"" desc:"A list of Network Service Requests" split_words:"true"`
}

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx := signalctx.WithSignals(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	logrus.SetFormatter(&nested.Formatter{})
	logger.EnableTracing(true)
	ctx, _ = logruslogger.New(
		logger.WithFields(ctx, map[string]interface{}{"cmd": os.Args[0]}),
	)
	// ********************************************************************************
	// Debug self if necessary
	// ********************************************************************************
	if err := debug.Self(); err != nil {
		logger.Log(ctx).Infof("%s", err)
	}
	starttime := time.Now()
	// enumerating phases
	logger.Log(ctx).Infof("there are 6 phases which will be executed followed by a success message:")
	logger.Log(ctx).Infof("the phases include:")
	logger.Log(ctx).Infof("1: get config from environment")
	logger.Log(ctx).Infof("2: run vpp and get a connection to it")
	logger.Log(ctx).Infof("3: retrieve spiffe svid")
	logger.Log(ctx).Infof("4: create grpc connection")
	logger.Log(ctx).Infof("5: create network service client")
	logger.Log(ctx).Infof("6: connect to all passed services")
	logger.Log(ctx).Infof("a final success message with start time duration")
	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 1: get config from environment (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now := time.Now()
	config := &Config{}
	if err := envconfig.Usage("nsm", config); err != nil {
		logrus.Fatal(err)
	}
	if err := envconfig.Process("nsm", config); err != nil {
		logrus.Fatalf("error processing config from env: %+v", err)
	}
	logger.Log(ctx).Infof("Config: %#v", config)
	logger.Log(ctx).WithField("duration", time.Since(now)).Infof("completed phase 1: get config from environment")
	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 2: run vpp and get a connection to it (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now = time.Now()
	vppConn, vppErrCh := vpphelper.StartAndDialContext(ctx)
	exitOnErrCh(ctx, cancel, vppErrCh)
	logger.Log(ctx).WithField("duration", time.Since(now)).Info("completed phase 2: run vpp and get a connection to it")
	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 3: retrieving svid, check spire agent logs if this is the last line you see (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	now = time.Now()
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)
	logger.Log(ctx).WithField("duration", time.Since(now)).Info("completed phase 3: retrieving svid")

	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 4: create grpc connection (time since start: %s)", time.Since(starttime))
	// ********************************************************************************

	dialCtx, cancel := context.WithTimeout(ctx, config.DialTimeout)
	defer cancel()

	clientCC, err := grpc.DialContext(
		dialCtx,
		grpcutils.URLToTarget(&config.ConnectTo),
		append(opentracing.WithTracingDial(),
			grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
			grpc.WithTransportCredentials(
				grpcfd.TransportCredentials(
					credentials.NewTLS(
						tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
					),
				),
			))...,
	)
	if err != nil {
		logrus.Fatalf("error getting clientCC: %+v", err)
	}

	var lastSocketID uint32

	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 5: create network service client (time since start: %s)", time.Since(starttime))
	// ********************************************************************************

	c := client.NewClient(
		ctx,
		config.Name,
		nil,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		clientCC,
		chain.NewNetworkServiceClient(
			metadata.NewClient(),
			up.NewClient(ctx, vppConn),
			connectioncontext.NewClient(vppConn),
			memif.NewClient(vppConn, &lastSocketID),
			sendfd.NewClient(),
			recvfd.NewClient(),
		),
	)

	var connects []*networkservice.Connection

	defer func() {
		for _, conn := range connects {
			closeCtx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
			defer cancel()
			_, _ = c.Close(closeCtx, conn)
		}
	}()

	// ********************************************************************************
	logger.Log(ctx).Infof("executing phase 6: connect to all passed services (time since start: %s)", time.Since(starttime))
	// ********************************************************************************

	for i := 0; i < len(config.NetworkServices); i++ {
		u := nsurl.NSURL(config.NetworkServices[i])
		mech := u.Mechanism()
		if mech.Type != memif.MECHANISM {
			logger.Log(ctx).Fatalf("mechanism type: %v is not supproted", mech.Type)
		}
		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				Id:             fmt.Sprintf("%v-%v", config.Name, uuid.New().String()),
				NetworkService: u.NetworkService(),
				Labels:         u.Labels(),
			},
		}
		requestCtx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
		defer cancel()
		_, err = c.Request(requestCtx, request)
		if err != nil {
			logger.Log(ctx).Fatalf("requset has failed: %v", err.Error())
		}
	}

	<-ctx.Done()
	<-vppErrCh
}

func exitOnErrCh(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		logger.Log(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		logger.Log(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}
