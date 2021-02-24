/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2021 Red Hat, Inc.
 *
 */

package network

// the hardcoded MAC must be as low as possible, to prevent libvirt from
// assigning a lower MAC to a tap device attached to the bridge - which
// would trigger the bridge's MAC to update. This also applies for the
// dummy connected to the bridge on masquerade binding.
const HardcodedMasqueradeMAC = "02:00:00:00:00:00"

type ReservedMac interface {
	IsReserved() bool
}

type Mac string

func (m Mac) IsReserved() bool {
	return m == HardcodedMasqueradeMAC
}
