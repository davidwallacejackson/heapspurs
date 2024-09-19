package treeclimber

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/adamroach/heapspurs/pkg/heapdump"
	"github.com/goccy/go-graphviz"
	"github.com/goccy/go-graphviz/cgraph"
)

type TreeClimber struct {
	params *heapdump.DumpParams

	records []heapdump.Record // All records in the heap dump

	memory                      map[uint64]heapdump.Record // Map of all records that represet an in-memory construct
	valueAddrsToTypeDescriptors map[uint64]*heapdump.TypeDescriptor
	valueAddrsToItabs           map[uint64]*heapdump.Itab                     // Map of all type descriptors
	typeDescriptorsToValues     map[heapdump.TypeDescriptor][]heapdump.Record // Map of type descriptor addresses to all values associated with them

	owners     map[uint64][]heapdump.Record // Maps from pointed-to objects to the thing(s) pointing to them
	visited    map[uint64]bool              // Temporary state used to keep track of already-visited nodes during graph traversal
	finalizers map[uint64]heapdump.Record   // Map of object address to its finalizer (if any)
}

func NewTreeClimber(reader *bufio.Reader) (*TreeClimber, error) {
	c := &TreeClimber{
		valueAddrsToTypeDescriptors: make(map[uint64]*heapdump.TypeDescriptor),
		valueAddrsToItabs:           make(map[uint64]*heapdump.Itab),
		typeDescriptorsToValues:     make(map[heapdump.TypeDescriptor][]heapdump.Record),
	}
	err := c.build(reader)
	if err != nil {
		return nil, err
	}

	c.annotate()

	return c, err
}

func (c *TreeClimber) PrintOwners(address uint64, depth int) error {
	c.visited = make(map[uint64]bool)
	defer func() { c.visited = nil }()
	if depth > 0 {
		depth++
	}
	return c.printOwners(address, depth)
}

func (c *TreeClimber) PrintAnchors(address uint64) error {
	c.visited = make(map[uint64]bool)
	defer func() { c.visited = nil }()
	return c.printAnchors(address)
}

func (c *TreeClimber) Hexdump(address uint64) (string, error) {
	r, found := c.memory[address]
	if !found {
		return "", fmt.Errorf("Cound not find record for address 0x%x", address)
	}

	o, isOwner := r.(heapdump.Owner)
	if !isOwner {
		return "", fmt.Errorf("Object of type %T does not have Contents", r)
	}

	ret := hex.Dump(o.GetContents())

	for _, field := range o.GetFields() {
		ret = ret + fmt.Sprintf("Pointer: 0x%x\n", field)
	}

	return ret, nil
}

func (c *TreeClimber) WritePNG(address uint64, w io.Writer) error {
	return c.WriteImage(address, w, graphviz.PNG)
}

func (c *TreeClimber) WriteSVG(address uint64, w io.Writer) error {
	return c.WriteImage(address, w, graphviz.SVG)
}

func (c *TreeClimber) WriteImage(address uint64, w io.Writer, format graphviz.Format) error {
	c.visited = make(map[uint64]bool)
	defer func() { c.visited = nil }()

	g := graphviz.New()
	defer g.Close()
	graph, err := g.Graph()
	if err != nil {
		return err
	}
	defer graph.Close()

	c.addNode(graph, address, true)

	fmt.Printf("Rendering graph (%d nodes)...\n", len(c.visited))
	return g.Render(graph, format, w)
}

// func (c *TreeClimber) Intersection(other *TreeClimber) *TreeClimber {
// 	intersection := TreeClimber{}
// }

///////////////////////////////////////////////////////////////////////////

func unitize(x uint64) string {
	switch {
	case x < 2*1024:
		return fmt.Sprintf("%d B", x)
	case x < 2*1024*1024:
		return fmt.Sprintf("%.f kiB", float64(x)/(1024))
	case x < 2*1024*1024*1024:
		return fmt.Sprintf("%.2f MiB", float64(x)/(1024*1024))
	case x < 2*1024*1024*1024*1024:
		return fmt.Sprintf("%.2f GiB", float64(x)/(1024*1024*1024))
	default:
		return fmt.Sprintf("%.2f TiB", float64(x)/(1024*1024*1024*1024))
	}
	return ""
}

// There are four owner types in a heap dump:
// Object
// StackFrame
// BssSegment
// DataSegment
func (c *TreeClimber) addNode(graph *cgraph.Graph, address uint64, spotlight bool) *cgraph.Node {
	record, found := c.memory[address]
	if !found {
		node, _ := graph.CreateNode(fmt.Sprintf("0x%x", address))
		node.SetLabel(fmt.Sprintf("???\n0x%x", address))
		node.SetShape(cgraph.PlainShape)
		if spotlight {
			node.SetStyle(cgraph.FilledNodeStyle)
			node.SetFillColor("yellow")
		}
		return node
	}

	if c.visited[address] {
		node, _ := graph.Node(fmt.Sprintf("0x%x", address))
		return node
	}
	c.visited[address] = true

	finalizer, _ := c.finalizers[address]

	node, _ := graph.CreateNode(fmt.Sprintf("0x%x", address))
	switch r := record.(type) {
	case *heapdump.Object:
		name := r.GetName()
		if name != "Object" {
			node.SetFontColor("#008000")
		}
		label := fmt.Sprintf("%s (%s)\n0x%x", name, unitize(uint64(len(r.Contents))), address)
		if finalizer != nil {
			label += fmt.Sprintf("\n%T", finalizer)
			node.SetColor("red")
			node.SetPenWidth(5)
		}
		node.SetLabel(label)
		node.SetShape(cgraph.EllipseShape)

		// Objects generally have owners; track them down and graph them.
		// Because owners can point to subfields within an object, we need to scan
		// for references anywhere inside the object.
		foundOwner := false
		end := uint64(len(r.Contents)) + address
		for dest := address; dest < end; dest++ {
			o, hasOwners := c.owners[dest]
			if hasOwners {
				for _, owner := range o {
					a, isOwner := owner.(heapdump.Owner)
					if isOwner {
						foundOwner = true
						on := c.addNode(graph, a.GetAddress(), false)
						edge, _ := graph.CreateEdge("", on, node)
						if dest != address {
							edge.SetHeadLabel(fmt.Sprintf("0x%x\n(offset = %d)", dest, dest-address))
							edge.SetColor("red")
						}
						ps := heapdump.GetPointersSourceAddress(a, dest, c.params)
						if ps != 0 {
							oidName := heapdump.GetName(ps)
							if oidName != "" {
								edge.SetTailLabel(oidName)
								println("found new name for %x, (%s -> %s)", r.Address, r.Name, oidName)
								// set the OID as this thing's name
								if r.Name != "" {
									r.Name = fmt.Sprintf("%s [orig: %s]", oidName, r.Name)
								} else {
									r.Name = oidName
								}
							}
						}
					}
				}
			}
		}
		if !foundOwner {
			node.SetStyle(cgraph.FilledNodeStyle)
			node.SetFillColor("gray")
		}
	case *heapdump.StackFrame:
		node.SetLabel(fmt.Sprintf("StackFrame @ 0x%x\n%s", address, c.fullStack(address, "\\l")+"\\l"))
		node.SetShape(cgraph.BoxShape)
	case *heapdump.BssSegment:
		node.SetLabel("BssSegment")
		node.SetShape(cgraph.DoubleOctagonShape)
	case *heapdump.DataSegment:
		node.SetLabel("DataSegment")
		node.SetShape(cgraph.TripleOctagonShape)
	default:
		node.SetLabel(fmt.Sprintf("%T\n0x%x", r, address))
		node.SetShape(cgraph.HouseShape)
	}
	if spotlight {
		node.SetStyle(cgraph.FilledNodeStyle)
		node.SetFillColor("yellow")
	}

	return node
}

func (c *TreeClimber) fullStack(address uint64, separator string) string {
	out := make([]string, 0)
	framePtr := address
	for framePtr != 0 {
		frameRecord, found := c.memory[framePtr]
		frame := frameRecord.(*heapdump.StackFrame)
		if !found {
			break
		}
		out = append(out, fmt.Sprintf("[%d] %s", frame.Depth, frame.Name))
		framePtr = frame.ChildPointer
	}
	return strings.Join(out, separator)
}

func (c *TreeClimber) printOwners(address uint64, depth int, prefix ...string) error {
	if depth == 0 {
		return nil
	}
	if c.visited[address] {
		return nil
		// return fmt.Errorf("Loop: already visited address 0x%x", address)
	}
	c.visited[address] = true
	r, found := c.memory[address]
	if !found {
		return fmt.Errorf("Cound not find record for address 0x%x", address)
	}
	indent := ""
	for _, p := range prefix {
		indent = indent + p
	}
	//fmt.Printf("%s%T @ 0x%x\n", indent, r, address)
	if o, isOwner := r.(heapdump.Owner); isOwner {
		var typeDescriptor *heapdump.TypeDescriptor

		if t := c.valueAddrsToTypeDescriptors[address]; t != nil {
			typeDescriptor = t
		} else if itab := c.valueAddrsToItabs[address]; itab != nil {
			typeDescriptor = c.valueAddrsToTypeDescriptors[itab.TypeDescriptorAddress]
		}

		var name string
		if typeDescriptor != nil {
			name = fmt.Sprintf(" (%s)", typeDescriptor.Name)
		} else if obj, isObject := r.(*heapdump.Object); isObject {
			name = fmt.Sprintf(" (%s)", obj.GetName())
		}

		output := fmt.Sprintf("%s%T%s @ 0x%x\n", indent, o, name, address)
		output = strings.Replace(output, "*heapdump.", "", 1)
		fmt.Print(output)
	} else {
		s, _ := r.(fmt.Stringer)
		fmt.Printf("%s%s\n", indent, s.String())
	}

	o, found := c.owners[address]
	if !found {
		return nil
	}
	for _, owner := range o {
		a, addressable := owner.(heapdump.Addressable)
		if addressable {
			err := c.printOwners(a.GetAddress(), depth-1, indent, "  ")
			if err != nil {
				fmt.Printf("%s  %v\n", indent, err)
			}
		}
	}
	return nil
}

func (c *TreeClimber) printAnchors(address uint64) error {
	if c.visited[address] {
		return fmt.Errorf("Loop: already visited address 0x%x", address)
	}
	c.visited[address] = true
	r, found := c.memory[address]
	if !found {
		return fmt.Errorf("Cound not find record for address 0x%x", address)
	}

	switch root := r.(type) {
	case *heapdump.OtherRoot:
		fmt.Println(root.String())
	case *heapdump.StackFrame:
		fmt.Println(root.String())
		childPtr := root.ChildPointer
		for childPtr != 0 {
			childRecord, found := c.memory[childPtr]
			child := childRecord.(*heapdump.StackFrame)
			if !found {
				return fmt.Errorf("Cound not find stack frame at address 0x%x", childPtr)
			}
			fmt.Printf("  %s\n", child.String())
			childPtr = child.ChildPointer
		}
	case *heapdump.BssSegment:
		fmt.Println(root.String())
	case *heapdump.DataSegment:
		fmt.Println(root.String())
	}

	o, found := c.owners[address]
	if !found {
		return nil
	}
	for _, owner := range o {
		a, addressable := owner.(heapdump.Addressable)
		if addressable {
			c.printAnchors(a.GetAddress())
		}
	}
	return nil
}

func getAs[R heapdump.Record](c *TreeClimber, address uint64) *R {
	r, found := c.memory[address]
	if !found {
		return nil
	}

	asType, isType := r.(R)
	if isType {
		return &asType
	}

	return nil
}

var eofRecordError = errors.New("EofRecord")

func (c *TreeClimber) addRecord(record heapdump.Record) error {
	switch r := record.(type) {
	case *heapdump.Eof:
		return eofRecordError
	case *heapdump.DumpParams:
		c.params = r
	case *heapdump.QueuedFinalizer:
		c.finalizers[r.ObjectAddress] = r
	case *heapdump.RegisteredFinalizer:
		c.finalizers[r.ObjectAddress] = r
	}

	a, isAddressable := record.(heapdump.Addressable)
	if isAddressable {
		c.memory[a.GetAddress()] = record
	}

	// Dump parameters isn't *defined* to come before other
	// records; but in practice, it does. If this changes,
	// we may need to move the construction of owner pointers
	// to after we read all of the records in the file.
	o, isOwner := record.(heapdump.Owner)
	if isOwner {
		pointers := heapdump.GetPointers(o, c.params)
		for i := 0; i < len(pointers); i++ {
			if pointers[i] != 0 {
				c.addOwner(pointers[i], record)
			}
		}
	}

	c.records = append(c.records, record)

	return nil
}

func (c *TreeClimber) build(reader *bufio.Reader) error {
	err := heapdump.ReadHeader(reader)
	if err != nil {
		return fmt.Errorf("Reading header: %w\n", err)
	}

	c.memory = make(map[uint64]heapdump.Record)
	c.owners = make(map[uint64][]heapdump.Record)
	c.finalizers = make(map[uint64]heapdump.Record)

	for {
		record, err := heapdump.ReadRecord(reader)
		if err != nil {
			return err
		}

		if err := c.addRecord(record); err != nil {
			if !errors.Is(err, eofRecordError) {
				println("weird error: %w", err)
			}

			break
		}
	}

	return nil
}

func (c *TreeClimber) addOwner(address uint64, r heapdump.Record) {
	_, found := c.owners[address]
	if !found {
		c.owners[address] = make([]heapdump.Record, 0)
	}

	c.owners[address] = append(c.owners[address], r)
}

func (c *TreeClimber) addFields(o heapdump.Owner) {
	var lastTypeDescriptor *heapdump.TypeDescriptor
	var lastItab *heapdump.Itab
	pointers := heapdump.GetPointers(o, c.params)

	for _, ptr := range pointers {
		if lastTypeDescriptor != nil {
			c.valueAddrsToTypeDescriptors[ptr] = lastTypeDescriptor
			c.typeDescriptorsToValues[*lastTypeDescriptor] = append(
				c.typeDescriptorsToValues[*lastTypeDescriptor],
				c.memory[ptr],
			)
			lastTypeDescriptor = nil
			lastItab = nil
			continue
		}
		if lastItab != nil {
			c.valueAddrsToItabs[ptr] = lastItab
			lastTypeDescriptor = nil
			lastItab = nil
			continue
		}

		tmpTypeDescriptor := getAs[*heapdump.TypeDescriptor](c, ptr)
		if tmpTypeDescriptor != nil {
			lastTypeDescriptor = *tmpTypeDescriptor
		}

		tmpItab := getAs[*heapdump.Itab](c, ptr)
		if tmpItab != nil {
			lastItab = *tmpItab
		}
	}
}

func (c *TreeClimber) annotate() {
	for _, r := range c.records {
		switch record := r.(type) {
		case heapdump.Owner:
			c.addFields(record)
		}
	}
}
