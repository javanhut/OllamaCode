package huffman

import (
	"container/heap"
	"encoding/base64"
	"encoding/binary"
	"strings"
)

type Node struct {
	Char  rune
	Freq  int
	Left  *Node
	Right *Node
}

type PriorityQueue []*Node

func (pq PriorityQueue) Len() int            { return len(pq) }
func (pq PriorityQueue) Less(i, j int) bool  { return pq[i].Freq < pq[j].Freq }
func (pq PriorityQueue) Swap(i, j int)       { pq[i], pq[j] = pq[j], pq[i] }
func (pq *PriorityQueue) Push(x interface{}) { *pq = append(*pq, x.(*Node)) }
func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

func BuildTree(text string) *Node {
	freqs := make(map[rune]int)
	for _, char := range text {
		freqs[char]++
	}

	pq := make(PriorityQueue, 0)
	for char, freq := range freqs {
		pq = append(pq, &Node{Char: char, Freq: freq})
	}
	heap.Init(&pq)

	if pq.Len() == 1 {
		child := heap.Pop(&pq).(*Node)
		return &Node{
			Freq: child.Freq,
			Left: child,
		}
	}

	for pq.Len() > 1 {
		left := heap.Pop(&pq).(*Node)
		right := heap.Pop(&pq).(*Node)
		parent := &Node{
			Freq:  left.Freq + right.Freq,
			Left:  left,
			Right: right,
		}
		heap.Push(&pq, parent)
	}

	if pq.Len() == 0 {
		return nil
	}
	return heap.Pop(&pq).(*Node)
}

func GetCodes(root *Node, currentCode string, codes map[rune]string) {
	if root == nil {
		return
	}
	if root.Left == nil && root.Right == nil {
		codes[root.Char] = currentCode
		return
	}
	GetCodes(root.Left, currentCode+"0", codes)
	GetCodes(root.Right, currentCode+"1", codes)
}

func Encode(text string) (string, *Node) {
	if text == "" {
		return "", nil
	}
	root := BuildTree(text)
	codes := make(map[rune]string)
	GetCodes(root, "", codes)

	var encoded strings.Builder
	for _, char := range text {
		encoded.WriteString(codes[char])
	}
	return encoded.String(), root
}

func Decode(encoded string, root *Node) string {
	if root == nil || encoded == "" {
		return ""
	}
	var decoded strings.Builder
	current := root
	for _, bit := range encoded {
		if bit == '0' {
			current = current.Left
		} else {
			current = current.Right
		}

		if current.Left == nil && current.Right == nil {
			decoded.WriteRune(current.Char)
			current = root
		}
	}
	return decoded.String()
}

type CompressedData struct {
	Encoded string `json:"encoded"`
	Tree    *Node  `json:"tree"`
}

func pack(bitStr string) []byte {
	n := len(bitStr)
	numBytes := 4 + (n+7)/8
	buf := make([]byte, numBytes)
	binary.BigEndian.PutUint32(buf[0:4], uint32(n))

	for i := 0; i < n; i++ {
		if bitStr[i] == '1' {
			buf[4+i/8] |= (1 << (7 - (i % 8)))
		}
	}
	return buf
}

func unpack(buf []byte) string {
	if len(buf) < 4 {
		return ""
	}
	n := int(binary.BigEndian.Uint32(buf[0:4]))
	expectedBytes := 4 + (n+7)/8
	if len(buf) < expectedBytes {
		n = (len(buf) - 4) * 8
		if n < 0 {
			return ""
		}
	}
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		b := buf[4+i/8]
		bit := (b >> (7 - (i % 8))) & 1
		if bit == 1 {
			sb.WriteByte('1')
		} else {
			sb.WriteByte('0')
		}
	}
	return sb.String()
}

func isLegacyBitString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' && s[i] != '1' {
			return false
		}
	}
	return true
}

func Compress(text string) (*CompressedData, error) {
	if text == "" {
		return &CompressedData{}, nil
	}
	encoded, root := Encode(text)
	packed := pack(encoded)
	b64 := base64.StdEncoding.EncodeToString(packed)
	return &CompressedData{
		Encoded: b64,
		Tree:    root,
	}, nil
}

func Decompress(data *CompressedData) string {
	if data == nil || data.Encoded == "" {
		return ""
	}
	if isLegacyBitString(data.Encoded) {
		return Decode(data.Encoded, data.Tree)
	}
	packed, err := base64.StdEncoding.DecodeString(data.Encoded)
	if err != nil {
		return Decode(data.Encoded, data.Tree)
	}
	bitStr := unpack(packed)
	return Decode(bitStr, data.Tree)
}
