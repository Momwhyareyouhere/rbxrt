package arm64emu

import (
	"debug/elf"
	"encoding/binary"
	"fmt"

	"rbxrt/internal/engine"
	"rbxrt/internal/hostabi"
)

// Emulator implements a basic ARM64 CPU for executing guest code.
type Emulator struct {
	// CPU registers (X0-X30, PC, SP)
	regs [32]uint64
	pc   uint64 // Program counter
	sp   uint64 // Stack pointer

	// Memory management
	mem      []byte
	memBase  uint64 // Base address of mapped memory
	memSize  uint64 // Size of memory mapping

	// ELF binary information
	eng *engine.Binary
}

// NewEmulator creates a new ARM64 emulator instance.
func NewEmulator(eng *engine.Binary) (*Emulator, error) {
	// Allocate 128MB of memory for guest code/data
	const memSize = 128 * 1024 * 1024
	mem := make([]byte, memSize)

	emu := &Emulator{
		mem:     mem,
		memBase: 0x4000000000, // Start at 1TB address (typical for Android)
		memSize: memSize,
		eng:     eng,
	}

	// Initialize stack pointer to top of memory
	emu.sp = emu.memBase + emu.memSize - 0x1000 // Leave some space at the top
	emu.regs[31] = emu.sp // X30 is SP in some conventions, but we'll use direct access

	return emu, nil
}

// LoadELF loads ELF sections into emulator memory.
func (e *Emulator) LoadELF() error {
	// Map all PT_LOAD segments
	for _, prog := range e.eng.File.Progs {
		if prog.Type != elf.PT_LOAD {
			continue
		}

		// Calculate guest virtual address range
		guestStart := prog.Vaddr
		guestEnd := prog.Vaddr + prog.Memsz

		// Calculate host memory addresses
		hostStart := guestStart + e.memBase
		hostEnd := guestEnd + e.memBase

		// Check bounds
		if hostStart < e.memBase || hostEnd > e.memBase+e.memSize {
			return fmt.Errorf("memory segment %#x-%#x out of bounds", guestStart, guestEnd)
		}

		// Map file contents
		if prog.Filesz > 0 {
			// Find the section that contains this program header's offset
			var data []byte
			var err error
			for _, sec := range e.eng.File.Sections {
				if sec.Off <= prog.Off && sec.Off+sec.Size > prog.Off {
					data, err = sec.Data()
					if err != nil {
						return fmt.Errorf("failed to read section data: %w", err)
					}
					break
				}
			}
			if data == nil {
				return fmt.Errorf("could not find section containing offset %#x", prog.Off)
			}

			fileOffset := prog.Off
			if int(fileOffset+prog.Filesz) > len(data) {
				return fmt.Errorf("file offset %#x+%#x exceeds section size", fileOffset, prog.Filesz)
			}

			copy(e.mem[hostStart-e.memBase:hostStart-e.memBase+prog.Filesz], data[fileOffset:fileOffset+prog.Filesz])
		}

		// Zero-fill BSS section
		if prog.Memsz > prog.Filesz {
			zeroStart := hostStart + prog.Filesz
			zeroSize := prog.Memsz - prog.Filesz
			for i := uint64(0); i < zeroSize; i++ {
				e.mem[zeroStart-e.memBase+i] = 0
			}
		}

		// Set memory permissions (simplified - in reality we'd mprotect)
		// For now we allow read/write/exec everywhere for simplicity
	}

	return nil
}

// EnterGuestCode prepares the emulator to execute code at the given address.
func (e *Emulator) EnterGuestCode(addr uintptr) {
	e.pc = uint64(addr) - e.memBase
}

// Run executes guest code until a halt condition or system call.
func (e *Emulator) Run() error {
	const maxSteps = 10000 // Prevent infinite loops during testing
	steps := 0

	for steps < maxSteps {
		// Fetch instruction (little-endian ARM64)
		if e.pc+4 > e.memSize {
			return fmt.Errorf("PC %#x out of memory bounds", e.pc)
		}

		instr := binary.LittleEndian.Uint32(e.mem[e.pc : e.pc+4])
		e.pc += 4

		// Decode and execute instruction
		err := e.executeInstruction(instr)
		if err != nil {
			return err
		}

		// Check for halt conditions (simplified)
		if e.pc == 0xFFFFFFFFFFFFFFFF {
			break
		}

		steps++
	}

	if steps >= maxSteps {
		return fmt.Errorf("exceeded maximum instruction steps (%d)", maxSteps)
	}

	return nil
}

// executeInstruction decodes and executes a single ARM64 instruction.
// This is a simplified implementation handling only a subset of instructions.
func (e *Emulator) executeInstruction(instr uint32) error {
	// Extract instruction fields
	opcode := instr >> 21 & 0x3FF
	rd := instr & 0x1F          // Destination register
	rn := (instr >> 5) & 0x1F   // Source register 1
	rm := (instr >> 16) & 0x1F  // Source register 2
	imm12 := instr >> 10 & 0xFFF  // 12-bit immediate
	imm26 := instr & 0x3FFFFFF    // 26-bit immediate

	// Handle common instruction types
	switch opcode >> 2 { // Major opcode groups
	case 0: // Data processing - immediate
		switch opcode & 0x3 {
		case 0: // ADD/SUB immediate
			if opcode&(1<<22) == 0 { // ADD
				e.regs[rd] = e.regs[rn] + uint64(imm12)
			} else { // SUB
				e.regs[rd] = e.regs[rn] - uint64(imm12)
			}
		case 1: // MOV to/from SP
			if opcode&(1<<23) == 0 { // MOV to SP
				e.sp = e.regs[rn] + uint64(imm12)<<12
			} else { // MOV from SP
				e.regs[rd] = e.sp - (uint64(imm12) << 12)
			}
		}
	case 1: // Data processing - register
		switch opcode & 0x3 {
		case 0: // ADD/SUB register
			if opcode&(1<<22) == 0 { // ADD
				if opcode&(1<<10) == 0 { // Register shift
					e.regs[rd] = e.regs[rn] + e.regs[rm]
				} else { // Immediate shift
					shift := e.regs[rm] & 0x3F
					if opcode&(1<<15) == 0 { // LSL
						e.regs[rd] = e.regs[rn] + (e.regs[rm] << shift)
					} else { // LSR
						e.regs[rd] = e.regs[rn] + (e.regs[rm] >> shift)
					}
				}
			} else { // SUB
				if opcode&(1<<10) == 0 { // Register shift
					e.regs[rd] = e.regs[rn] - e.regs[rm]
				} else { // Immediate shift
					shift := e.regs[rm] & 0x3F
					if opcode&(1<<15) == 0 { // LSL
						e.regs[rd] = e.regs[rn] - (e.regs[rm] << shift)
					} else { // LSR
						e.regs[rd] = e.regs[rn] - (e.regs[rm] >> shift)
					}
				}
			}
		case 1: // Logical register
			switch opcode & 0x3 {
			case 0: // AND
				e.regs[rd] = e.regs[rn] & e.regs[rm]
			case 1: // ORR
				e.regs[rd] = e.regs[rn] | e.regs[rm]
			case 2: // EOR
				e.regs[rd] = e.regs[rn] ^ e.regs[rm]
			case 3: // ANDS (flags)
				e.regs[rd] = e.regs[rn] & e.regs[rm]
			}
		}
	case 2: // Branches and system
		if (instr>>24)&0x3 == 0 && (instr>>26)&0x3 == 0 { // Unconditional branch
			// Sign-extend 26-bit immediate
			offset := int64(imm26<<2) << 6 >> 6 // Sign extend
			e.pc += uint64(offset)
		} else if (instr>>24)&0xF == 0xD && (instr>>0)&0xFFFFF == 0xD400000 { // System call
			return e.handleSystemCall()
		}
	default:
		// For unimplemented instructions, just continue (NOP-like behavior)
		// In a real emulator, we'd decode more instructions or return an error
	}

	return nil
}

// handleSystemCall processes a system call made by guest code.
func (e *Emulator) handleSystemCall() error {
	// In ARM64, system calls are made via SVC instruction
	// X8 contains the system call number
	// X0-X6 contain arguments
	// Return value in X0

	syscallNum := e.regs[8] // X8
	args := []uint64{
		e.regs[0],  // X0
		e.regs[1],  // X1
		e.regs[2],  // X2
		e.regs[3],  // X3
		e.regs[4],  // X4
		e.regs[5],  // X5
		e.regs[6],  // X6
		e.regs[7],  // X7
	}

	// Handle common system calls by delegating to hostabi
	switch syscallNum {
	case 0:  // read
		if len(args) >= 3 {
			fd := int(args[0])
			buf := e.mem[args[1]-e.memBase : args[1]-e.memBase+args[2]]
			n, err := hostabi.Read(fd, buf)
			e.regs[0] = uint64(n)
			if err != nil {
				e.regs[0] = ^uint64(0) // Return -1 on error
			}
		}
	case 1:  // write
		if len(args) >= 3 {
			fd := int(args[0])
			buf := e.mem[args[1]-e.memBase : args[1]-e.memBase+args[2]]
			n, err := hostabi.Write(fd, buf)
			e.regs[0] = uint64(n)
			if err != nil {
				e.regs[0] = ^uint64(0) // Return -1 on error
			}
		}
	case 57: // fork (not supported)
		e.regs[0] = ^uint64(0)
	case 56: // execve (not supported)
		e.regs[0] = ^uint64(0)
	case 60: // exit
		if len(args) >= 1 {
			// Set halt condition
			e.pc = 0xFFFFFFFFFFFFFFFF
			e.regs[0] = args[0]
		}
	default:
		// Unsupported syscall - return ENOSYS
		e.regs[0] = ^uint64(0) // -1
	}

	return nil
}

// ReadMem reads memory from the guest address space.
func (e *Emulator) ReadMem(addr uintptr, size int) ([]byte, error) {
	hostAddr := uint64(addr) - e.memBase
	if hostAddr < 0 || hostAddr+uint64(size) > e.memSize {
		return nil, fmt.Errorf("memory access %#x+%d out of bounds", addr, size)
	}
	return e.mem[hostAddr : hostAddr+uint64(size)], nil
}

// WriteMem writes memory to the guest address space.
func (e *Emulator) WriteMem(addr uintptr, data []byte) error {
	hostAddr := uint64(addr) - e.memBase
	if hostAddr < 0 || hostAddr+uint64(len(data)) > e.memSize {
		return fmt.Errorf("memory access %#x+%d out of bounds", addr, len(data))
	}
	copy(e.mem[hostAddr:hostAddr+uint64(len(data))], data)
	return nil
}

// GetReg returns the value of a guest register.
func (e *Emulator) GetReg(reg int) uint64 {
	if reg < 0 || reg > 30 {
		return 0
	}
	return e.regs[reg]
}

// SetReg sets the value of a guest register.
func (e *Emulator) SetReg(reg int, val uint64) {
	if reg >= 0 && reg <= 30 {
		e.regs[reg] = val
	}
}

// GetPC returns the current program counter.
func (e *Emulator) GetPC() uintptr {
	return uintptr(e.pc + e.memBase)
}

// SetPC sets the program counter.
func (e *Emulator) SetPC(addr uintptr) {
	e.pc = uint64(addr) - e.memBase
}