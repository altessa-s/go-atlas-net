// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

// Package drivertest provides local-server conformance tests for transport
// drivers. Run it from each driver's own test package. Passing is regression
// evidence, not containment: review every socket-opening path in the driver and
// its dependencies to ensure it uses DriverEnv.Dial.
package drivertest
