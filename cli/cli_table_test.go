package main

import (
	"bytes"
	"fmt"
	"testing"
)

func TestTableAlignsChineseAndASCII(t *testing.T) {
	var out bytes.Buffer
	table := newCLITable(&out)
	fmt.Fprintln(table, "节点\t状态")
	fmt.Fprintln(table, "a\t在线")
	fmt.Fprintln(table, "中文名\t离线")
	if err := table.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "节点    状态\na       在线\n中文名  离线\n"
	if out.String() != want {
		t.Fatalf("%q != %q", out.String(), want)
	}
	if cliTextWidth("e\u0301") != 1 || cliTextWidth("—") != 1 {
		t.Fatal("combining/wide width")
	}
}
