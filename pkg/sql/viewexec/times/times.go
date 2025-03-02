// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package times

import (
	"bytes"
	"unsafe"

	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/hashtable"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/sql/util"
	"github.com/matrixorigin/matrixone/pkg/sql/viewexec/transform"
	"github.com/matrixorigin/matrixone/pkg/vectorize/add"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

func String(_ interface{}, buf *bytes.Buffer) {
	buf.WriteString(" ⨯  ")
}

func Prepare(proc *process.Process, arg interface{}) error {
	n := arg.(*Argument)
	n.ctr = new(Container)
	transform.Prepare(proc, n.Arg)
	n.ctr.state = Fill
	n.ctr.views = make([]*view, len(n.Ss))
	n.ctr.keyOffs = make([]uint32, UnitLimit)
	n.ctr.zKeyOffs = make([]uint32, UnitLimit)
	n.ctr.hashes = make([]uint64, UnitLimit)
	n.ctr.strHashStates = make([][3]uint64, UnitLimit)
	n.ctr.values = make([]uint64, UnitLimit)
	n.ctr.h8.keys = make([]uint64, UnitLimit)
	for i := 0; i < len(n.Ss); i++ {
		n.ctr.views[i] = &view{
			isB: false,
			rn:  n.Ss[i],
			key: n.Svars[i],
		}
	}
	n.ctr.constructVars(n)
	return nil
}

func Call(proc *process.Process, arg interface{}) (bool, error) {
	n := arg.(*Argument)
	if n.ctr.state == End {
		proc.Reg.InputBatch = nil
		return true, nil
	}
	if n.ctr.state == Fill {
		if err := n.ctr.fill(n.Bats, proc); err != nil {
			proc.Reg.InputBatch = nil
			n.ctr.state = End
			return true, err
		}
		n.ctr.isB = true
		for _, v := range n.ctr.views {
			if !v.isB {
				n.ctr.isB = false
			}
		}
		if !n.ctr.isB {
			panic("no possible")
		}
		n.ctr.state = Probe
	}
	if _, err := transform.Call(proc, n.Arg); err != nil {
		n.ctr.state = End
		proc.Reg.InputBatch = nil
		return false, err
	}
	bat := proc.Reg.InputBatch
	if bat == nil {
		n.ctr.state = End
		if n.ctr.pctr == nil {
			return true, nil
		}
		switch n.ctr.pctr.typ {
		case H8:
			n.ctr.bat.Ht = n.ctr.pctr.intHashMap
		case H24:
			n.ctr.bat.Ht = n.ctr.pctr.strHashMap
		case H32:
			n.ctr.bat.Ht = n.ctr.pctr.strHashMap
		case H40:
			n.ctr.bat.Ht = n.ctr.pctr.strHashMap
		default:
			n.ctr.bat.Ht = n.ctr.pctr.strHashMap
		}
		proc.Reg.InputBatch = n.ctr.bat
		n.ctr.bat = nil
		return true, nil
	}
	if len(bat.Zs) == 0 {
		proc.Reg.InputBatch = &batch.Batch{}
		return false, nil
	}
	if err := n.ctr.probe(n.Arg.Ctr.Is, n.FreeVars, bat, n, proc); err != nil {
		proc.Reg.InputBatch = nil
		n.ctr.state = End
		return true, err
	}
	proc.Reg.InputBatch = &batch.Batch{}
	return false, nil
}

func (ctr *Container) fill(bats []*batch.Batch, proc *process.Process) error {
	for i := 0; i < len(bats); i++ {
		bat := bats[i]
		if bat == nil {
			continue
		}
		if len(bat.Zs) == 0 {
			i--
			continue
		}
		if err := ctr.fillBatch(ctr.views[i], bat, proc); err != nil {
			return err
		}
	}
	return nil
}

func (ctr *Container) probe(is []int, freeVars []string, bat *batch.Batch, arg *Argument, proc *process.Process) error {
	defer batch.Clean(bat, proc.Mp)
	if ctr.pctr == nil {
		ctr.constructBatch(arg.R, arg.VarsMap, freeVars, bat)
		ctr.pctr.values = make([]uint64, UnitLimit)
	} else {
		batch.Reorder(bat, ctr.pctr.attrs)
	}
	switch ctr.pctr.typ {
	case H8:
		return ctr.probeH8(is, arg, bat, proc)
	case H24:
		return ctr.probeH24(is, arg, bat, proc)
	case H32:
		return ctr.probeH32(is, arg, bat, proc)
	case H40:
		return ctr.probeH40(is, arg, bat, proc)
	default:
		return ctr.probeHstr(is, arg, bat, proc)
	}
}

func (ctr *Container) probeH8(is []int, arg *Argument, bat *batch.Batch, proc *process.Process) error {
	vecs := make([]*vector.Vector, len(arg.Rvars))
	{
		for vi := 0; vi < len(arg.Rvars); vi++ {
			vecs[vi] = batch.GetVector(bat, arg.Rvars[vi])
		}
	}
	gvecs := make([]*vector.Vector, len(ctr.pctr.freeVars))
	{
		for i, fidx := range ctr.pctr.freeIndexs {
			if fidx[0] == -1 {
				gvecs[i] = bat.Vecs[fidx[1]]
			} else {
				gvecs[i] = ctr.views[fidx[0]].bat.Vecs[fidx[1]]
			}
		}
	}
	values := make([][]uint64, len(arg.Rvars))
	{
		for i := 0; i < len(arg.Rvars); i++ {
			values[i] = make([]uint64, UnitLimit)
		}
	}
	var strKeys [UnitLimit][]byte
	var strKeys16 [UnitLimit][16]byte
	var zStrKeys16 [UnitLimit][16]byte
	count := int64(len(bat.Zs))
	for i := int64(0); i < count; i += UnitLimit {
		n := count - i
		if n > UnitLimit {
			n = UnitLimit
		}
		for vi := 0; vi < len(arg.Rvars); vi++ {
			v := ctr.views[vi]
			switch vecs[vi].Typ.Oid {
			case types.T_int8:
				vs := vecs[vi].Col.([]int8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int16:
				vs := vecs[vi].Col.([]int16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int32:
				vs := vecs[vi].Col.([]int32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_date:
				vs := vecs[vi].Col.([]types.Date)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int64:
				vs := vecs[vi].Col.([]int64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_datetime:
				vs := vecs[vi].Col.([]types.Datetime)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint8:
				vs := vecs[vi].Col.([]uint8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint16:
				vs := vecs[vi].Col.([]uint16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint32:
				vs := vecs[vi].Col.([]uint32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint64:
				vs := vecs[vi].Col.([]uint64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float32:
				vs := vecs[vi].Col.([]float32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float64:
				vs := vecs[vi].Col.([]float64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_char, types.T_varchar:
				vs := vecs[vi].Col.(*types.Bytes)
				var padded int
				for k := int64(0); k < n; k++ {
					if vs.Lengths[i+k] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+k))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + k)
					}
				}
				v.strHashMap.FindStringBatch(ctr.strHashStates, strKeys[:n], values[vi])
				copy(strKeys16[:padded], zStrKeys16[:padded])
			}
		}
		copy(ctr.keyOffs, ctr.zKeyOffs)
		copy(ctr.h8.keys, ctr.h8.zKeys)
		{
			data := unsafe.Slice((*byte)(unsafe.Pointer(&ctr.h8.keys[0])), cap(ctr.h8.keys)*8)[:len(ctr.h8.keys)*8]
			for j, vec := range gvecs {
				if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
					vps := values[vi]
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int32(vs[0])
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int32(vs[vp-1])
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int64(vs[0])
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int64(vs[vp-1])
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								key := vs.Get(0)
								copy(data[k*8+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							} else {
								key := vs.Get(int64(vp) - 1)
								copy(data[k*8+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							}
						}
					}
				} else {
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int32(vs[i+k])
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int64(vs[i+k])
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							key := vs.Get(i + k)
							copy(data[k*8+int64(ctr.keyOffs[k]):], key)
							ctr.keyOffs[k] += uint32(len(key))
						}
					}
				}
			}
		}
		ctr.hashes[0] = 0
		{
			for k := int64(0); k < n; k++ {
				o := i + int64(k)
				z := bat.Zs[o]
				for vi, vps := range values {
					if vps[k] == 0 {
						z = 0
						break
					}
					if !ctr.views[vi].isOne {
						z *= ctr.views[vi].bat.Zs[vps[k]-1]
					}
				}
				ctr.pctr.zs[k] = z
			}
		}
		ctr.pctr.intHashMap.InsertBatchWithRing(int(n), ctr.pctr.zs, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
		for k, v := range ctr.values[:n] {
			if ctr.pctr.zs[k] == 0 {
				continue
			}
			o := i + int64(k)
			if v > ctr.pctr.rows {
				for j, vec := range ctr.bat.Vecs {
					idx := o
					if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
						if vp := values[vi][k]; vp > 0 {
							idx = int64(vp) - 1
						} else {
							idx = 0
						}
					}
					if err := vector.UnionOne(vec, gvecs[j], idx, proc.Mp); err != nil {
						return err
					}
				}
				ctr.pctr.rows++
				for _, r := range ctr.bat.Rs {
					if err := r.Grow(proc.Mp); err != nil {
						return err
					}
				}
				ctr.bat.Zs = append(ctr.bat.Zs, 0)
			}
			ai := int64(v) - 1
			for j := range bat.Rs {
				ctr.bat.Rs[j].Fill(ai, o, ctr.pctr.zs[k]/bat.Zs[o], bat.Vecs[is[j]])
			}
			for vi, vps := range values {
				sel := vps[k] - 1
				v := ctr.views[vi]
				{ // ring fill
					for j, r := range v.bat.Rs {
						ctr.bat.Rs[v.ris[j]].Mul(r, ai, int64(sel), ctr.pctr.zs[k]/v.bat.Zs[sel])
					}
				}
			}
			ctr.bat.Zs[ai] += ctr.pctr.zs[k]
		}
	}
	return nil
}

func (ctr *Container) probeH24(is []int, arg *Argument, bat *batch.Batch, proc *process.Process) error {
	vecs := make([]*vector.Vector, len(arg.Rvars))
	{
		for vi := 0; vi < len(arg.Rvars); vi++ {
			vecs[vi] = batch.GetVector(bat, arg.Rvars[vi])
		}
	}
	gvecs := make([]*vector.Vector, len(ctr.pctr.freeVars))
	{
		for i, fidx := range ctr.pctr.freeIndexs {
			if fidx[0] == -1 {
				gvecs[i] = bat.Vecs[fidx[1]]
			} else {
				gvecs[i] = ctr.views[fidx[0]].bat.Vecs[fidx[1]]
			}
		}
	}
	values := make([][]uint64, len(arg.Rvars))
	{
		for i := 0; i < len(arg.Rvars); i++ {
			values[i] = make([]uint64, UnitLimit)
		}
	}
	var strKeys [UnitLimit][]byte
	var strKeys16 [UnitLimit][16]byte
	var zStrKeys16 [UnitLimit][16]byte
	count := int64(len(bat.Zs))
	for i := int64(0); i < count; i += UnitLimit {
		n := count - i
		if n > UnitLimit {
			n = UnitLimit
		}
		for vi := 0; vi < len(arg.Rvars); vi++ {
			v := ctr.views[vi]
			switch vecs[vi].Typ.Oid {
			case types.T_int8:
				vs := vecs[vi].Col.([]int8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int16:
				vs := vecs[vi].Col.([]int16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int32:
				vs := vecs[vi].Col.([]int32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int64:
				vs := vecs[vi].Col.([]int64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint8:
				vs := vecs[vi].Col.([]uint8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint16:
				vs := vecs[vi].Col.([]uint16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint32:
				vs := vecs[vi].Col.([]uint32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_date:
				vs := vecs[vi].Col.([]types.Date)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint64:
				vs := vecs[vi].Col.([]uint64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float32:
				vs := vecs[vi].Col.([]float32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float64:
				vs := vecs[vi].Col.([]float64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_datetime:
				vs := vecs[vi].Col.([]types.Datetime)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_char, types.T_varchar:
				vs := vecs[vi].Col.(*types.Bytes)
				var padded int
				for k := int64(0); k < n; k++ {
					if vs.Lengths[i+k] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+k))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + k)
					}
				}
				v.strHashMap.FindStringBatch(ctr.strHashStates, strKeys[:n], values[vi])
				copy(strKeys16[:padded], zStrKeys16[:padded])
			}
		}
		copy(ctr.keyOffs, ctr.zKeyOffs)
		copy(ctr.h24.keys, ctr.h24.zKeys)
		{
			data := unsafe.Slice((*byte)(unsafe.Pointer(&ctr.h24.keys[0])), cap(ctr.h24.keys)*24)[:len(ctr.h24.keys)*24]
			for j, vec := range gvecs {
				if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
					vps := values[vi]
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int32(vs[0])
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int32(vs[vp-1])
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int64(vs[0])
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int64(vs[vp-1])
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								key := vs.Get(0)
								copy(data[k*24+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							} else {
								key := vs.Get(int64(vp) - 1)
								copy(data[k*24+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							}
						}
					}
				} else {
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int32(vs[i+k])
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h24.keys[k]), ctr.keyOffs[k])) = int64(vs[i+k])
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							key := vs.Get(i + k)
							copy(data[k*24+int64(ctr.keyOffs[k]):], key)
							ctr.keyOffs[k] += uint32(len(key))
						}
					}
				}
			}
		}
		{
			for k := int64(0); k < n; k++ {
				o := i + int64(k)
				z := bat.Zs[o]
				for vi, vps := range values {
					if vps[k] == 0 {
						z = 0
						break
					}
					if !ctr.views[vi].isOne {
						z *= ctr.views[vi].bat.Zs[vps[k]-1]
					}
				}
				ctr.pctr.zs[k] = z
			}
		}
		ctr.pctr.strHashMap.InsertString24BatchWithRing(ctr.pctr.zs, ctr.strHashStates, ctr.h24.keys[:n], ctr.values)
		for k, v := range ctr.values[:n] {
			if ctr.pctr.zs[k] == 0 {
				continue
			}
			o := i + int64(k)
			if v > ctr.pctr.rows {
				for j, vec := range ctr.bat.Vecs {
					idx := o
					if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
						if vp := values[vi][k]; vp > 0 {
							idx = int64(vp) - 1
						} else {
							idx = 0
						}
					}
					if err := vector.UnionOne(vec, gvecs[j], idx, proc.Mp); err != nil {
						return err
					}
				}
				ctr.pctr.rows++
				for _, r := range ctr.bat.Rs {
					if err := r.Grow(proc.Mp); err != nil {
						return err
					}
				}
				ctr.bat.Zs = append(ctr.bat.Zs, 0)
			}
			ai := int64(v) - 1
			for j := range bat.Rs {
				ctr.bat.Rs[j].Fill(ai, o, ctr.pctr.zs[k]/bat.Zs[o], bat.Vecs[is[j]])
			}
			for vi, vps := range values {
				sel := vps[k] - 1
				v := ctr.views[vi]
				{ // ring fill
					for j, r := range v.bat.Rs {
						ctr.bat.Rs[v.ris[j]].Mul(r, ai, int64(sel), ctr.pctr.zs[k]/v.bat.Zs[sel])
					}
				}
			}
			ctr.bat.Zs[ai] += ctr.pctr.zs[k]
		}
	}
	return nil
}

func (ctr *Container) probeH32(is []int, arg *Argument, bat *batch.Batch, proc *process.Process) error {
	vecs := make([]*vector.Vector, len(arg.Rvars))
	{
		for vi := 0; vi < len(arg.Rvars); vi++ {
			vecs[vi] = batch.GetVector(bat, arg.Rvars[vi])
		}
	}
	gvecs := make([]*vector.Vector, len(ctr.pctr.freeVars))
	{
		for i, fidx := range ctr.pctr.freeIndexs {
			if fidx[0] == -1 {
				gvecs[i] = bat.Vecs[fidx[1]]
			} else {
				gvecs[i] = ctr.views[fidx[0]].bat.Vecs[fidx[1]]
			}
		}
	}
	values := make([][]uint64, len(arg.Rvars))
	{
		for i := 0; i < len(arg.Rvars); i++ {
			values[i] = make([]uint64, UnitLimit)
		}
	}
	var strKeys [UnitLimit][]byte
	var strKeys16 [UnitLimit][16]byte
	var zStrKeys16 [UnitLimit][16]byte
	count := int64(len(bat.Zs))
	for i := int64(0); i < count; i += UnitLimit {
		n := count - i
		if n > UnitLimit {
			n = UnitLimit
		}
		for vi := 0; vi < len(arg.Rvars); vi++ {
			v := ctr.views[vi]
			switch vecs[vi].Typ.Oid {
			case types.T_int8:
				vs := vecs[vi].Col.([]int8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int16:
				vs := vecs[vi].Col.([]int16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int32:
				vs := vecs[vi].Col.([]int32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int64:
				vs := vecs[vi].Col.([]int64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint8:
				vs := vecs[vi].Col.([]uint8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint16:
				vs := vecs[vi].Col.([]uint16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint32:
				vs := vecs[vi].Col.([]uint32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_date:
				vs := vecs[vi].Col.([]types.Date)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint64:
				vs := vecs[vi].Col.([]uint64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float32:
				vs := vecs[vi].Col.([]float32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float64:
				vs := vecs[vi].Col.([]float64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_datetime:
				vs := vecs[vi].Col.([]types.Datetime)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_char, types.T_varchar:
				vs := vecs[vi].Col.(*types.Bytes)
				var padded int
				for k := int64(0); k < n; k++ {
					if vs.Lengths[i+k] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+k))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + k)
					}
				}
				v.strHashMap.FindStringBatch(ctr.strHashStates, strKeys[:n], values[vi])
				copy(strKeys16[:padded], zStrKeys16[:padded])
			}
		}
		copy(ctr.keyOffs, ctr.zKeyOffs)
		copy(ctr.h32.keys, ctr.h32.zKeys)
		{
			data := unsafe.Slice((*byte)(unsafe.Pointer(&ctr.h32.keys[0])), cap(ctr.h32.keys)*32)[:len(ctr.h32.keys)*32]
			for j, vec := range gvecs {
				if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
					vps := values[vi]
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int32(vs[0])
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int32(vs[vp-1])
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int64(vs[0])
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int64(vs[vp-1])
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								key := vs.Get(0)
								copy(data[k*32+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							} else {
								key := vs.Get(int64(vp) - 1)
								copy(data[k*32+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							}
						}
					}
				} else {
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int32(vs[i+k])
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h32.keys[k]), ctr.keyOffs[k])) = int64(vs[i+k])
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							key := vs.Get(i + k)
							copy(data[k*24+int64(ctr.keyOffs[k]):], key)
							ctr.keyOffs[k] += uint32(len(key))
						}
					}
				}
			}
		}
		{
			for k := int64(0); k < n; k++ {
				o := i + int64(k)
				z := bat.Zs[o]
				for vi, vps := range values {
					if vps[k] == 0 {
						z = 0
						break
					}
					if !ctr.views[vi].isOne {
						z *= ctr.views[vi].bat.Zs[vps[k]-1]
					}
				}
				ctr.pctr.zs[k] = z
			}
		}
		ctr.pctr.strHashMap.InsertString32BatchWithRing(ctr.pctr.zs, ctr.strHashStates, ctr.h32.keys[:n], ctr.values)
		for k, v := range ctr.values[:n] {
			if ctr.pctr.zs[k] == 0 {
				continue
			}
			o := i + int64(k)
			if v > ctr.pctr.rows {
				for j, vec := range ctr.bat.Vecs {
					idx := o
					if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
						if vp := values[vi][k]; vp > 0 {
							idx = int64(vp) - 1
						} else {
							idx = 0
						}
					}
					if err := vector.UnionOne(vec, gvecs[j], idx, proc.Mp); err != nil {
						return err
					}
				}
				ctr.pctr.rows++
				for _, r := range ctr.bat.Rs {
					if err := r.Grow(proc.Mp); err != nil {
						return err
					}
				}
				ctr.bat.Zs = append(ctr.bat.Zs, 0)
			}
			ai := int64(v) - 1
			for j := range bat.Rs {
				ctr.bat.Rs[j].Fill(ai, o, ctr.pctr.zs[k]/bat.Zs[o], bat.Vecs[is[j]])
			}
			for vi, vps := range values {
				sel := vps[k] - 1
				v := ctr.views[vi]
				{ // ring fill
					for j, r := range v.bat.Rs {
						ctr.bat.Rs[v.ris[j]].Mul(r, ai, int64(sel), ctr.pctr.zs[k]/v.bat.Zs[sel])
					}
				}
			}
			ctr.bat.Zs[ai] += ctr.pctr.zs[k]
		}
	}
	return nil
}

func (ctr *Container) probeH40(is []int, arg *Argument, bat *batch.Batch, proc *process.Process) error {
	vecs := make([]*vector.Vector, len(arg.Rvars))
	{
		for vi := 0; vi < len(arg.Rvars); vi++ {
			vecs[vi] = batch.GetVector(bat, arg.Rvars[vi])
		}
	}
	gvecs := make([]*vector.Vector, len(ctr.pctr.freeVars))
	{
		for i, fidx := range ctr.pctr.freeIndexs {
			if fidx[0] == -1 {
				gvecs[i] = bat.Vecs[fidx[1]]
			} else {
				gvecs[i] = ctr.views[fidx[0]].bat.Vecs[fidx[1]]
			}
		}
	}
	values := make([][]uint64, len(arg.Rvars))
	{
		for i := 0; i < len(arg.Rvars); i++ {
			values[i] = make([]uint64, UnitLimit)
		}
	}
	var strKeys [UnitLimit][]byte
	var strKeys16 [UnitLimit][16]byte
	var zStrKeys16 [UnitLimit][16]byte
	count := int64(len(bat.Zs))
	for i := int64(0); i < count; i += UnitLimit {
		n := count - i
		if n > UnitLimit {
			n = UnitLimit
		}
		for vi := 0; vi < len(arg.Rvars); vi++ {
			v := ctr.views[vi]
			switch vecs[vi].Typ.Oid {
			case types.T_int8:
				vs := vecs[vi].Col.([]int8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int16:
				vs := vecs[vi].Col.([]int16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int32:
				vs := vecs[vi].Col.([]int32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int64:
				vs := vecs[vi].Col.([]int64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint8:
				vs := vecs[vi].Col.([]uint8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint16:
				vs := vecs[vi].Col.([]uint16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint32:
				vs := vecs[vi].Col.([]uint32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint64:
				vs := vecs[vi].Col.([]uint64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float32:
				vs := vecs[vi].Col.([]float32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_date:
				vs := vecs[vi].Col.([]types.Date)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float64:
				vs := vecs[vi].Col.([]float64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_datetime:
				vs := vecs[vi].Col.([]types.Datetime)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_char, types.T_varchar:
				vs := vecs[vi].Col.(*types.Bytes)
				var padded int
				for k := int64(0); k < n; k++ {
					if vs.Lengths[i+k] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+k))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + k)
					}
				}
				v.strHashMap.FindStringBatch(ctr.strHashStates, strKeys[:n], values[vi])
				copy(strKeys16[:padded], zStrKeys16[:padded])
			}
		}
		copy(ctr.keyOffs, ctr.zKeyOffs)
		copy(ctr.h40.keys, ctr.h40.zKeys)
		{
			data := unsafe.Slice((*byte)(unsafe.Pointer(&ctr.h40.keys[0])), cap(ctr.h40.keys)*40)[:len(ctr.h40.keys)*40]
			for j, vec := range gvecs {
				if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
					vps := values[vi]
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = int32(vs[0])
							} else {
								*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = int32(vs[vp-1])
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[0]
							} else {
								*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[vp-1]
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = int64(vs[0])
							} else {
								*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = int64(vs[vp-1])
							}
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								key := vs.Get(0)
								copy(data[k*40+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							} else {
								key := vs.Get(int64(vp) - 1)
								copy(data[k*40+int64(ctr.keyOffs[k]):], key)
								ctr.keyOffs[k] += uint32(len(key))
							}
						}
					}
				} else {
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						for k := int64(0); k < n; k++ {
							*(*int8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						for k := int64(0); k < n; k++ {
							*(*uint8)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						for k := int64(0); k < n; k++ {
							*(*int16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						for k := int64(0); k < n; k++ {
							*(*uint16)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(2, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						for k := int64(0); k < n; k++ {
							*(*uint32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						for k := int64(0); k < n; k++ {
							*(*float32)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						for k := int64(0); k < n; k++ {
							*(*int32)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int32(vs[i+k])
						}
						add.Uint32AddScalar(4, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						for k := int64(0); k < n; k++ {
							*(*uint64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						for k := int64(0); k < n; k++ {
							*(*float64)(unsafe.Add(unsafe.Pointer(&ctr.h40.keys[k]), ctr.keyOffs[k])) = vs[i+k]
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						for k := int64(0); k < n; k++ {
							*(*int64)(unsafe.Add(unsafe.Pointer(&ctr.h8.keys[k]), ctr.keyOffs[k])) = int64(vs[i+k])
						}
						add.Uint32AddScalar(8, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							key := vs.Get(i + k)
							copy(data[k*40+int64(ctr.keyOffs[k]):], key)
							ctr.keyOffs[k] += uint32(len(key))
						}
					}
				}
			}
		}
		{
			for k := int64(0); k < n; k++ {
				o := i + int64(k)
				z := bat.Zs[o]
				for vi, vps := range values {
					if vps[k] == 0 {
						z = 0
						break
					}
					if !ctr.views[vi].isOne {
						z *= ctr.views[vi].bat.Zs[vps[k]-1]
					}
				}
				ctr.pctr.zs[k] = z
			}
		}
		ctr.pctr.strHashMap.InsertString40BatchWithRing(ctr.pctr.zs, ctr.strHashStates, ctr.h40.keys[:n], ctr.values)
		for k, v := range ctr.values[:n] {
			if ctr.pctr.zs[k] == 0 {
				continue
			}
			o := i + int64(k)
			if v > ctr.pctr.rows {
				for j, vec := range ctr.bat.Vecs {
					idx := o
					if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
						if vp := values[vi][k]; vp > 0 {
							idx = int64(vp) - 1
						} else {
							idx = 0
						}
					}
					if err := vector.UnionOne(vec, gvecs[j], idx, proc.Mp); err != nil {
						return err
					}
				}
				ctr.pctr.rows++
				for _, r := range ctr.bat.Rs {
					if err := r.Grow(proc.Mp); err != nil {
						return err
					}
				}
				ctr.bat.Zs = append(ctr.bat.Zs, 0)
			}
			ai := int64(v) - 1
			for j := range bat.Rs {
				ctr.bat.Rs[j].Fill(ai, o, ctr.pctr.zs[k]/bat.Zs[o], bat.Vecs[is[j]])
			}
			for vi, vps := range values {
				sel := vps[k] - 1
				v := ctr.views[vi]
				{ // ring fill
					for j, r := range v.bat.Rs {
						ctr.bat.Rs[v.ris[j]].Mul(r, ai, int64(sel), ctr.pctr.zs[k]/v.bat.Zs[sel])
					}
				}
			}
			ctr.bat.Zs[ai] += ctr.pctr.zs[k]
		}
	}
	return nil
}

func (ctr *Container) probeHstr(is []int, arg *Argument, bat *batch.Batch, proc *process.Process) error {
	vecs := make([]*vector.Vector, len(arg.Rvars))
	{
		for vi := 0; vi < len(arg.Rvars); vi++ {
			vecs[vi] = batch.GetVector(bat, arg.Rvars[vi])
		}
	}
	gvecs := make([]*vector.Vector, len(ctr.pctr.freeVars))
	{
		for i, fidx := range ctr.pctr.freeIndexs {
			if fidx[0] == -1 {
				gvecs[i] = bat.Vecs[fidx[1]]
			} else {
				gvecs[i] = ctr.views[fidx[0]].bat.Vecs[fidx[1]]
			}
		}
	}
	values := make([][]uint64, len(arg.Rvars))
	{
		for i := 0; i < len(arg.Rvars); i++ {
			values[i] = make([]uint64, UnitLimit)
		}
	}
	var strKeys [UnitLimit][]byte
	var strKeys16 [UnitLimit][16]byte
	var zStrKeys16 [UnitLimit][16]byte
	count := int64(len(bat.Zs))
	for i := int64(0); i < count; i += UnitLimit {
		n := count - i
		if n > UnitLimit {
			n = UnitLimit
		}
		for vi := 0; vi < len(arg.Rvars); vi++ {
			v := ctr.views[vi]
			switch vecs[vi].Typ.Oid {
			case types.T_int8:
				vs := vecs[vi].Col.([]int8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int16:
				vs := vecs[vi].Col.([]int16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int32:
				vs := vecs[vi].Col.([]int32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_int64:
				vs := vecs[vi].Col.([]int64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint8:
				vs := vecs[vi].Col.([]uint8)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint16:
				vs := vecs[vi].Col.([]uint16)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint32:
				vs := vecs[vi].Col.([]uint32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_date:
				vs := vecs[vi].Col.([]types.Date)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_uint64:
				vs := vecs[vi].Col.([]uint64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float32:
				vs := vecs[vi].Col.([]float32)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_float64:
				vs := vecs[vi].Col.([]float64)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_datetime:
				vs := vecs[vi].Col.([]types.Datetime)
				for k := int64(0); k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[i+k])
				}
				ctr.hashes[0] = 0
				v.intHashMap.FindBatch(int(n), ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), values[vi])
			case types.T_char, types.T_varchar:
				vs := vecs[vi].Col.(*types.Bytes)
				var padded int
				for k := int64(0); k < n; k++ {
					if vs.Lengths[i+k] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+k))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + k)
					}
				}
				v.strHashMap.FindStringBatch(ctr.strHashStates, strKeys[:n], values[vi])
				copy(strKeys16[:padded], zStrKeys16[:padded])
			}
		}
		{
			for j, vec := range gvecs {
				if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
					vps := values[vi]
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := gvecs[j].Col.([]int8)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*1)[:len(vs)*1]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:1]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*1:vp*1]...)
							}
						}
					case types.T_uint8:
						vs := gvecs[j].Col.([]uint8)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*1)[:len(vs)*1]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:1]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*1:vp*1]...)
							}
						}
						add.Uint32AddScalar(1, ctr.keyOffs[:n], ctr.keyOffs[:n])
					case types.T_int16:
						vs := gvecs[j].Col.([]int16)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*2)[:len(vs)*2]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:2]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*2:vp*2]...)
							}
						}
					case types.T_uint16:
						vs := gvecs[j].Col.([]uint16)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*2)[:len(vs)*2]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:2]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*2:vp*2]...)
							}
						}
					case types.T_int32:
						vs := gvecs[j].Col.([]int32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:4]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*4:vp*4]...)
							}
						}
					case types.T_uint32:
						vs := gvecs[j].Col.([]uint32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:4]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*4:vp*4]...)
							}
						}
					case types.T_date:
						vs := gvecs[j].Col.([]types.Date)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:4]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*4:vp*4]...)
							}
						}
					case types.T_float32:
						vs := gvecs[j].Col.([]float32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:4]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*4:vp*4]...)
							}
						}
					case types.T_int64:
						vs := gvecs[j].Col.([]int64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:8]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*8:vp*8]...)
							}
						}
					case types.T_datetime:
						vs := gvecs[j].Col.([]types.Datetime)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:8]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*8:vp*8]...)
							}
						}
					case types.T_uint64:
						vs := gvecs[j].Col.([]uint64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:8]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*8:vp*8]...)
							}
						}
					case types.T_float64:
						vs := gvecs[j].Col.([]float64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[0:8]...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(vp-1)*8:vp*8]...)
							}
						}
					case types.T_char, types.T_varchar:
						vs := gvecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							if vp := vps[k]; vp == 0 {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], vs.Get(0)...)
							} else {
								ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], vs.Get(int64(vp)-1)...)
							}
						}
					}
				} else {
					switch vec.Typ.Oid {
					case types.T_int8:
						vs := vecs[j].Col.([]int8)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*1)[:len(vs)*1]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*1:(i+k+1)*1]...)
						}
					case types.T_uint8:
						vs := vecs[j].Col.([]uint8)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*1)[:len(vs)*1]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*1:(i+k+1)*1]...)
						}
					case types.T_int16:
						vs := vecs[j].Col.([]int16)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*2)[:len(vs)*2]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*2:(i+k+1)*2]...)
						}
					case types.T_uint16:
						vs := vecs[j].Col.([]uint16)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*2)[:len(vs)*2]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*2:(i+k+1)*2]...)
						}
					case types.T_int32:
						vs := vecs[j].Col.([]int32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*4:(i+k+1)*4]...)
						}
					case types.T_uint32:
						vs := vecs[j].Col.([]uint32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*4:(i+k+1)*4]...)
						}
					case types.T_date:
						vs := vecs[j].Col.([]types.Date)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*4:(i+k+1)*4]...)
						}
					case types.T_float32:
						vs := vecs[j].Col.([]float32)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*4)[:len(vs)*4]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*4:(i+k+1)*4]...)
						}
					case types.T_int64:
						vs := vecs[j].Col.([]int64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*8:(i+k+1)*8]...)
						}
					case types.T_uint64:
						vs := vecs[j].Col.([]uint64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*8:(i+k+1)*8]...)
						}
					case types.T_float64:
						vs := vecs[j].Col.([]float64)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*8:(i+k+1)*8]...)
						}
					case types.T_datetime:
						vs := vecs[j].Col.([]types.Datetime)
						data := unsafe.Slice((*byte)(unsafe.Pointer(&vs[0])), cap(vs)*8)[:len(vs)*8]
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], data[(i+k)*8:(i+k+1)*8]...)
						}
					case types.T_char, types.T_varchar:
						vs := vecs[j].Col.(*types.Bytes)
						for k := int64(0); k < n; k++ {
							ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], vs.Get(i+k)...)
						}
					}
				}
			}
		}
		{
			for k := int64(0); k < n; k++ {
				o := i + int64(k)
				z := bat.Zs[o]
				for vi, vps := range values {
					if vps[k] == 0 {
						z = 0
						break
					}
					if !ctr.views[vi].isOne {
						z *= ctr.views[vi].bat.Zs[vps[k]-1]
					}
				}
				ctr.pctr.zs[k] = z
			}
		}
		for k := int64(0); k < n; k++ {
			if l := len(ctr.pctr.hstr.keys[k]); l < 16 {
				ctr.pctr.hstr.keys[k] = append(ctr.pctr.hstr.keys[k], hashtable.StrKeyPadding[l:]...)
			}
		}
		ctr.pctr.strHashMap.InsertStringBatchWithRing(ctr.pctr.zs, ctr.strHashStates, ctr.pctr.hstr.keys[:n], ctr.values)
		for k, v := range ctr.values[:n] {
			ctr.pctr.hstr.keys[k] = ctr.pctr.hstr.keys[k][:0]
			if ctr.pctr.zs[k] == 0 {
				continue
			}
			o := i + int64(k)
			if v > ctr.pctr.rows {
				for j, vec := range ctr.bat.Vecs {
					idx := o
					if vi := ctr.pctr.freeIndexs[j][0]; vi >= 0 {
						if vp := values[vi][k]; vp > 0 {
							idx = int64(vp) - 1
						} else {
							idx = 0
						}
					}
					if err := vector.UnionOne(vec, gvecs[j], idx, proc.Mp); err != nil {
						return err
					}
				}
				ctr.pctr.rows++
				for _, r := range ctr.bat.Rs {
					if err := r.Grow(proc.Mp); err != nil {
						return err
					}
				}
				ctr.bat.Zs = append(ctr.bat.Zs, 0)
			}
			ai := int64(v) - 1
			for j := range bat.Rs {
				ctr.bat.Rs[j].Fill(ai, o, ctr.pctr.zs[k]/bat.Zs[o], bat.Vecs[is[j]])
			}
			for vi, vps := range values {
				sel := vps[k] - 1
				v := ctr.views[vi]
				{ // ring fill
					for j, r := range v.bat.Rs {
						ctr.bat.Rs[v.ris[j]].Mul(r, ai, int64(sel), ctr.pctr.zs[k]/v.bat.Zs[sel])
					}
				}
			}
			ctr.bat.Zs[ai] += ctr.pctr.zs[k]
		}
	}
	return nil
}

func (ctr *Container) fillBatch(v *view, bat *batch.Batch, proc *process.Process) error {
	v.bat = bat
	v.rows = 0
	vec := batch.GetVector(bat, v.key)
	switch v.typ = vec.Typ; v.typ.Oid {
	case types.T_int8:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]int8)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_int16:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]int16)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_int32:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]int32)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_date:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]types.Date)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_datetime:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]types.Datetime)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_int64:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]int64)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}

	case types.T_uint8:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]uint8)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_uint16:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]uint16)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_uint32:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]uint32)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_uint64:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]uint64)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_float32:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]float32)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_float64:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.intHashMap = v.bat.Ht.(*hashtable.Int64HashMap)
			return nil
		}
		flg := true
		v.intHashMap = &hashtable.Int64HashMap{}
		v.intHashMap.Init()
		vs := vec.Col.([]float64)
		count := int64(len(bat.Zs))
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			{
				for k := 0; k < n; k++ {
					ctr.h8.keys[k] = uint64(vs[int(i)+k])
				}
			}
			ctr.hashes[0] = 0
			v.intHashMap.InsertBatch(n, ctr.hashes, unsafe.Pointer(&ctr.h8.keys[0]), ctr.values)
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	case types.T_char, types.T_varchar:
		if v.bat.Ht != nil {
			v.isB = true
			v.isOne = true
			for _, z := range v.bat.Zs {
				if z > 1 {
					v.isOne = false
				}
			}
			v.strHashMap = v.bat.Ht.(*hashtable.StringHashMap)
			return nil
		}
		flg := true
		v.strHashMap = &hashtable.StringHashMap{}
		v.strHashMap.Init()
		vs := vec.Col.(*types.Bytes)
		count := int64(len(bat.Zs))
		var strKeys [UnitLimit][]byte
		var strKeys16 [UnitLimit][16]byte
		var zStrKeys16 [UnitLimit][16]byte
		for i := int64(0); i < count; i += UnitLimit {
			n := int(count - i)
			if n > UnitLimit {
				n = UnitLimit
			}
			var padded int
			{
				for k := 0; k < n; k++ {
					if vs.Lengths[i+int64(k)] < 16 {
						copy(strKeys16[padded][:], vs.Get(i+int64(k)))
						strKeys[k] = strKeys16[padded][:]
						padded++
					} else {
						strKeys[k] = vs.Get(i + int64(k))
					}
				}
			}
			ctr.hashes[0] = 0
			v.strHashMap.InsertStringBatch(ctr.strHashStates, strKeys[:n], ctr.values)
			copy(strKeys16[:padded], zStrKeys16[:padded])
			for k, vv := range ctr.values[:n] {
				if vv > v.rows {
					v.rows++
					v.sels = append(v.sels, make([]int64, 0, 8))
				}
				ai := int64(vv) - 1
				v.sels[ai] = append(v.sels[ai], i+int64(k))
				if len(v.sels[ai]) > 1 {
					flg = false
				}
			}
		}
		if flg { // reinsert
			v.isB = true
		}
	}
	return nil
}

func (ctr *Container) constructBatch(rn string, varsMap map[string]int, freeVars []string, bat *batch.Batch) {
	var size int

	ctr.pctr = new(probeContainer)
	ctr.pctr.freeVars = freeVars
	ctr.pctr.zs = make([]int64, UnitLimit)
	ctr.pctr.freeIndexs = make([][2]int, len(freeVars))
	ctr.pctr.attrs = append(ctr.pctr.attrs, bat.Attrs...)
	ctr.bat = batch.New(true, freeVars)
	ctr.sels = make([][]int64, len(freeVars))
	for i, fvar := range freeVars {
		ctr.sels[i] = make([]int64, UnitLimit)
		tbl, name := util.SplitTableAndColumn(fvar)
		if len(tbl) > 0 {
			if tbl == rn {
				idx := batch.GetVectorIndex(bat, name)
				vec := bat.Vecs[idx]
				switch vec.Typ.Oid {
				case types.T_int8:
					size += 1
				case types.T_int16:
					size += 2
				case types.T_int32:
					size += 4
				case types.T_int64:
					size += 8
				case types.T_uint8:
					size += 1
				case types.T_uint16:
					size += 2
				case types.T_uint32:
					size += 4
				case types.T_uint64:
					size += 8
				case types.T_float32:
					size += 4
				case types.T_float64:
					size += 8
				case types.T_char, types.T_varchar:
					if width := vec.Typ.Width; width > 0 {
						size += int(width)
					} else {
						size = 128
					}
				}
				ctr.bat.Vecs[i] = vector.New(vec.Typ)
				ctr.bat.Vecs[i].Ref = vec.Ref
				ctr.pctr.freeIndexs[i] = [2]int{-1, idx}
			} else {
				for vi, v := range ctr.views {
					if v.rn == tbl {
						idx := batch.GetVectorIndex(v.bat, name)
						vec := v.bat.Vecs[idx]
						switch vec.Typ.Oid {
						case types.T_int8:
							size += 1
						case types.T_int16:
							size += 2
						case types.T_int32:
							size += 4
						case types.T_int64:
							size += 8
						case types.T_uint8:
							size += 1
						case types.T_uint16:
							size += 2
						case types.T_uint32:
							size += 4
						case types.T_uint64:
							size += 8
						case types.T_float32:
							size += 4
						case types.T_float64:
							size += 8
						case types.T_char, types.T_varchar:
							if width := vec.Typ.Width; width > 0 {
								size += int(width)
							} else {
								size = 128
							}
						}
						ctr.bat.Vecs[i] = vector.New(vec.Typ)
						ctr.bat.Vecs[i].Ref = vec.Ref
						ctr.pctr.freeIndexs[i] = [2]int{vi, idx}
						break
					}
				}
			}
		} else {
			flg := false
			for vi, v := range ctr.views {
				if idx := batch.GetVectorIndex(v.bat, name); idx >= 0 {
					flg = true
					vec := v.bat.Vecs[idx]
					switch vec.Typ.Oid {
					case types.T_int8:
						size += 1
					case types.T_int16:
						size += 2
					case types.T_int32:
						size += 4
					case types.T_int64:
						size += 8
					case types.T_uint8:
						size += 1
					case types.T_uint16:
						size += 2
					case types.T_uint32:
						size += 4
					case types.T_uint64:
						size += 8
					case types.T_float32:
						size += 4
					case types.T_float64:
						size += 8
					case types.T_char, types.T_varchar:
						if width := vec.Typ.Width; width > 0 {
							size += int(width)
						} else {
							size = 128
						}
					}
					ctr.bat.Vecs[i] = vector.New(vec.Typ)
					ctr.bat.Vecs[i].Ref = vec.Ref
					ctr.pctr.freeIndexs[i] = [2]int{vi, idx}
					break
				}
			}
			if !flg {
				if idx := batch.GetVectorIndex(bat, name); idx >= 0 {
					vec := bat.Vecs[idx]
					switch vec.Typ.Oid {
					case types.T_int8:
						size += 1
					case types.T_int16:
						size += 2
					case types.T_int32:
						size += 4
					case types.T_int64:
						size += 8
					case types.T_uint8:
						size += 1
					case types.T_uint16:
						size += 2
					case types.T_uint32:
						size += 4
					case types.T_uint64:
						size += 8
					case types.T_float32:
						size += 4
					case types.T_float64:
						size += 8
					case types.T_char, types.T_varchar:
						if width := vec.Typ.Width; width > 0 {
							size += int(width)
						} else {
							size = 128
						}
					}
					ctr.bat.Vecs[i] = vector.New(vec.Typ)
					ctr.bat.Vecs[i].Ref = vec.Ref
					ctr.pctr.freeIndexs[i] = [2]int{-1, idx}
				}
			}
		}
	}
	switch {
	case size <= 8:
		ctr.pctr.typ = H8
		ctr.h8.keys = make([]uint64, UnitLimit)
		ctr.h8.zKeys = make([]uint64, UnitLimit)
		ctr.pctr.intHashMap = &hashtable.Int64HashMap{}
		ctr.pctr.intHashMap.Init()
	case size <= 24:
		ctr.pctr.typ = H24
		ctr.h24.keys = make([][3]uint64, UnitLimit)
		ctr.h24.zKeys = make([][3]uint64, UnitLimit)
		ctr.pctr.strHashMap = &hashtable.StringHashMap{}
		ctr.pctr.strHashMap.Init()
	case size <= 32:
		ctr.pctr.typ = H32
		ctr.h32.keys = make([][4]uint64, UnitLimit)
		ctr.h32.zKeys = make([][4]uint64, UnitLimit)
		ctr.pctr.strHashMap = &hashtable.StringHashMap{}
		ctr.pctr.strHashMap.Init()
	case size <= 40:
		ctr.pctr.typ = H40
		ctr.h40.keys = make([][5]uint64, UnitLimit)
		ctr.h40.zKeys = make([][5]uint64, UnitLimit)
		ctr.pctr.strHashMap = &hashtable.StringHashMap{}
		ctr.pctr.strHashMap.Init()
	default:
		ctr.pctr.typ = HStr
		ctr.pctr.hstr.keys = make([][]byte, UnitLimit)
		ctr.pctr.strHashMap = &hashtable.StringHashMap{}
		ctr.pctr.strHashMap.Init()
	}
	for i, r := range bat.Rs {
		ctr.bat.Rs = append(ctr.bat.Rs, r.Dup())
		ctr.bat.As = append(ctr.bat.As, bat.As[i])
		ctr.bat.Refs = append(ctr.bat.Refs, bat.Refs[i])
	}
	for _, v := range ctr.views {
		for i, r := range v.bat.Rs {
			v.ris = append(v.ris, len(ctr.bat.Rs))
			ctr.bat.Rs = append(ctr.bat.Rs, r.Dup())
			ctr.bat.As = append(ctr.bat.As, v.bat.As[i])
			ctr.bat.Refs = append(ctr.bat.Refs, v.bat.Refs[i])
		}
	}
}

func (ctr *Container) constructVars(arg *Argument) {
	ctr.vars = make([]string, 0, 8)
	ctr.fvarsMap = make(map[string]uint8)
	for _, fv := range arg.Arg.FreeVars {
		ctr.fvarsMap[fv] = 0
	}
	for _, rvar := range arg.Rvars {
		if arg.VarsMap[rvar] > 1 {
			ctr.vars = append(ctr.vars, arg.R+"."+rvar)
		} else {
			ctr.vars = append(ctr.vars, rvar)
		}
	}
	for i, s := range arg.Ss {
		if arg.VarsMap[arg.Svars[i]] > 1 {
			ctr.vars = append(ctr.vars, s+"."+arg.Svars[i])
		} else {
			ctr.vars = append(ctr.vars, arg.Svars[i])
		}
	}
	ctr.varsMap = make(map[string]uint8)
	for _, v := range ctr.vars {
		ctr.varsMap[v] = 0
	}
}

func (ctr *Container) calculateCount(z int64, vzs [][]int64, vsels [][]int64) {

	if len(vsels) == 0 {
		ctr.zs = append(ctr.zs, z)
		return
	}

	rowNum := int64(len(vsels))
	colNum := int64(len(vsels[0]))

	N := int64(1)
	a := colNum
	for i := rowNum; i > 0; i >>= 1 {
		if i&1 != 0 {
			N *= a
		}
		a *= a
	}

	placeIndex := N

	if int64(cap(ctr.intermediateBuffer)) < N {
		ctr.intermediateBuffer = append(ctr.intermediateBuffer, make([]int64, N-int64(cap(ctr.intermediateBuffer)))...)
	}

	for i := colNum - 1; i >= 0; i-- {
		ctr.intermediateBuffer[placeIndex-1] = z * vzs[rowNum-1][vsels[rowNum-1][i]]
		placeIndex--
	}

	for index := rowNum - 1; index >= 0; index-- {
		if placeIndex == 0 {
			break
		}
		l := N - placeIndex
		s := N - l*colNum
		for j := int64(0); j < colNum; j++ {
			for i := placeIndex; i < N; i++ {
				ctr.intermediateBuffer[s] = ctr.intermediateBuffer[i] * vzs[index-1][vsels[index-1][j]]
				s++
			}
		}
		placeIndex = N - l*colNum
	}
	ctr.intermediateBuffer = ctr.intermediateBuffer[:N]

	ctr.zs = append(ctr.zs, ctr.intermediateBuffer...)
}
