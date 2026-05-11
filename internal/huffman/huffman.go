package huffman

import (
	"container/heap"
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

	encoded := ""
	for _, char := range text {
		encoded += codes[char]
	}
	return encoded, root
}

func Decode(encoded string, root *Node) string {
	if root == nil || encoded == "" {
		return ""
	}
	decoded := ""
	current := root
	for _, bit := range encoded {
		if bit == '0' {
			current = current.Left
		} else {
			current = current.Right
		}

		if current.Left == nil && current.Right == nil {
			decoded += string(current.Char)
			current = root
		}
	}
	return decoded
}

type CompressedData struct {
	Encoded string         `json:"encoded"`
	Tree    *Node          `json:"tree"`
}

func Compress(text string) (*CompressedData, error) {
	if text == "" {
		return &CompressedData{}, nil
	}
	encoded, root := Encode(text)
	return &CompressedData{
		Encoded: encoded,
		Tree:    root,
	}, nil
}

func Decompress(data *CompressedData) string {
	if data == nil || data.Encoded == "" {
		return ""
	}
	return Decode(data.Encoded, data.Tree)
}
