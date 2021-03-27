// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
)

func WithSignal(ctx context.Context, s ...os.Signal) (context.Context, context.CancelFunc) {
	sigCtx, cancel := context.WithCancel(ctx)
	c := make(chan os.Signal, 1)
	signal.Notify(c, s...)
	go func() {
		select {
		case <-c:
			log.Printf("Received %s, exiting", s)
			cancel()
		case <-ctx.Done():
			cancel()
		}
		signal.Stop(c)
	}()
	return sigCtx, cancel
}

func WithWaitGroup(ctx context.Context, wg *sync.WaitGroup) context.Context {
	wgCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		wg.Wait()
		cancel()
	}()
	return wgCtx
}
