package models

import "log"

type Model struct {
	name string
	tag  string
}

func (m *Model) New() {
	if m.name == "" {
		log.Println("Model name is not set: " + m.name + " setting to empty.")
		m.name = ""
	}
	if m.tag == "" {
		log.Println("Tag not set. setting default to latest")
		m.tag = "latest"
	}
}

func (m Model) GetModelName() string {
	return m.name
}

func (m Model) GetModelNumber() string {
	return m.tag
}

func (m *Model) SetModelName(name string) {
	m.name = name
}

func (m *Model) SetTag(tag string) {
	m.tag = tag
}
