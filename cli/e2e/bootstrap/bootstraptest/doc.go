// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bootstraptest runs `nvkectl bootstrap site` for the bootstrap e2e
// lanes.
//
// It owns starting and supervising the command, and the assertions moved here
// from the lanes so far: VerifySiteStack checks the NVKE stack on the site
// cluster, VerifyPivot that its management plane moved there, and SecretScan
// checks that the OTLP header credential and the secrets bootstrap generates
// stay out of its output. It adds no lane: cli/e2e/bootstrap/run.sh and
// run-nico.sh remain the
// two lanes, they still build the fixtures, bridge the networks and make the
// rest of the assertions about what the run produced, by watching the log file
// and grepping it afterwards. New assertions belong here rather than in the
// lanes. TestE2E_BootstrapSite stands exactly
// where `setsid nvkectl bootstrap site … &` stood, and the lane waits on it the
// same way.
//
// Supervising the command is the part that does not belong in shell. nvkectl
// does not forward signals to the kind, helm and nerdctl it drives, so the
// command needs a process group of its own and the group needs signalling on
// the way out, which is what the lanes used setsid for.
//
// # Marking a test as needing a cluster
//
// A test here that drives a real bootstrap carries two marks, because they do
// different jobs:
//
//   - The e2e_bootstrap build tag keeps it out of an ordinary build. `make
//     test` compiles this package and runs its unit tests without the tag, so a
//     tagged test is not merely skipped, it is not built. That is what stops a
//     unit test run from trying to bootstrap a cluster.
//   - The TestE2E_ name prefix selects it once the tag is on. The lanes pass
//     -run '^TestE2E_', so a test added with that prefix is picked up
//     without touching the shell, and a helper or unit test added beside it is
//     not.
//
// Use both. The tag alone leaves nothing to select with, and the prefix alone
// would let `go test ./...` run a bootstrap.
package bootstraptest
