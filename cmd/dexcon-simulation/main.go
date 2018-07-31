// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/dexon-foundation/dexon-consensus-core/simulation"
	"github.com/dexon-foundation/dexon-consensus-core/simulation/config"
)

var initialize = flag.Bool("init", false, "initialize config file")
var configFile = flag.String("config", "", "path to simulation config file")

func main() {
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	if *configFile == "" {
		fmt.Fprintln(os.Stderr, "error: no configuration file specified")
		os.Exit(1)
	}

	if *initialize {
		if err := config.GenerateDefault(*configFile); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s", err)
			os.Exit(1)
		}
		//os.Exit(0)
	}

	simulation.Run(*configFile)
}