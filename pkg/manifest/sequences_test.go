package manifest

import (
	"skima/pkg/util"
	"testing"

	"github.com/spf13/viper"
)

func int64p(n int64) *int64 { return &n }

func TestSequenceDdl(t *testing.T) {
	full := Sequence{Name: "invoice_no", Datatype: "integer", Start: int64p(1000), Increment: int64p(-5), Minvalue: int64p(-1000000),
		Maxvalue: int64p(5000), Cache: int64p(20), Cycle: true, Ownedby: "invoices.number"}
	bare := Sequence{Name: "plain"}
	cases := map[string]string{
		full.GenerateDdl(): "create sequence invoice_no as integer increment by -5 minvalue -1000000 maxvalue 5000 start with 1000 cache 20 cycle",
		bare.GenerateDdl(): "create sequence plain",
		full.OwnedByDdl():  "alter sequence invoice_no owned by invoices.number",
		// an alter sets every setting - one the state file leaves out goes back to the default - and never restarts it
		full.AlterDdl(): "alter sequence invoice_no as integer increment by -5 minvalue -1000000 maxvalue 5000 cache 20 cycle owned by invoices.number",
		bare.AlterDdl(): "alter sequence plain as bigint increment by 1 no minvalue no maxvalue cache 1 no cycle owned by none",
		CommentDdl("sequence", "plain", "Plain numbers"): "comment on sequence plain is 'Plain numbers'",
	}
	for actual, expected := range cases {
		if actual != expected {
			t.Errorf("EXPECTED %s\n  ACTUAL: %s", expected, actual)
		}
	}
}

func TestSequenceDelta(t *testing.T) {
	reader = osReadWrapper{}
	viper.Set("workdir", writeManifest(t, map[string]string{
		"state/invoice_no-sequence.json": `{"name":"invoice_no","start":1000,"increment":5,"ownedby":"invoices.number","comment":"Invoice numbers"}`,
	}))
	t.Cleanup(func() { viper.Set("workdir", "") })
	cases := []struct {
		delta    SimpleDelta
		expected string
	}{
		{SimpleDelta{Object: "sequence", Action: "add", Name: "invoice_no"}, "create sequence invoice_no increment by 5 start with 1000"},
		{SimpleDelta{Object: "sequence", Action: "alter", Name: "invoice_no"}, "alter sequence invoice_no as bigint increment by 5 no minvalue no maxvalue cache 1 no cycle owned by invoices.number"},
		{SimpleDelta{Object: "sequence", Action: "drop", Name: "invoice_no"}, "drop sequence if exists invoice_no"},
		{SimpleDelta{Object: "comment", Action: "add", On: "sequence", Name: "invoice_no"}, "comment on sequence invoice_no is 'Invoice numbers'"},
	}
	for _, c := range cases {
		if ddl, err := c.delta.Ddl(); err != nil || ddl != c.expected {
			t.Errorf("%s: EXPECTED %s  ACTUAL: %s, %v", c.delta.Describe(), c.expected, ddl, err)
		}
	}
	if _, err := (SimpleDelta{Object: "sequence", Action: "alter", Name: "absent"}).Ddl(); err == nil {
		t.Error("EXPECTED an error for a sequence without a state file")
	}
}

func TestSequenceValidation(t *testing.T) {
	reader = osReadWrapper{}
	workdir := writeManifest(t, map[string]string{
		"state/invoices-table.json":      `{"name":"invoices","columns":[{"name":"id","datatype":"uuid"},{"name":"number","datatype":"bigint"}]}`,
		"state/invoice_no-sequence.json": `{"name":"invoice_no","ownedby":"invoices.number"}`,
		"state/renamed-sequence.json":    `{"name":"other"}`,
		"state/no_table-sequence.json":   `{"name":"no_table","ownedby":"missing.id"}`,
		"state/no_column-sequence.json":  `{"name":"no_column","ownedby":"invoices.total"}`,
		"state/bad-sequence.json":        `{"name":"bad","datatype":"text","increment":0,"cache":0,"ownedby":"number","restart":1}`,
		"deltas/v1_0_1.json": `{"deltas":[
			{"object":"sequence","action":"add","name":"invoice_no"},
			{"object":"sequence","action":"alter","name":"absent"},
			{"object":"sequence","action":"drop","name":"absent"},
			{"object":"comment","action":"add","on":"sequence","name":"invoice_no"}
		]}`,
		"deltas/v1_0_2.json": `{"deltas":[{"object":"sequence","action":"restart","name":"invoice_no","value":"1"}]}`,
	})
	viper.Set("workdir", workdir)
	t.Cleanup(func() { viper.Set("workdir", "") })
	since := util.ExtractVersion("1.0.0")
	results, err := ValidateManifest(workdir, &since)
	if err != nil {
		t.Fatal(err)
	}
	expectIssues(t, results, "invoice_no-sequence.json")
	expectIssues(t, results, "renamed-sequence.json", "error /name: name 'other' does not match the file name - deltas load this file as sequence 'renamed'")
	expectIssues(t, results, "no_table-sequence.json", "error /ownedby: owned by table 'missing', which has no valid state file")
	expectIssues(t, results, "no_column-sequence.json", "error /ownedby: owned by column 'total', which table 'invoices' does not have in state/invoices-table.json")
	expectIssues(t, results, "bad-sequence.json",
		"error : additional properties 'restart' not allowed",
		"error /cache: minimum",
		"error /datatype: value must be one of 'smallint', 'integer', 'bigint'",
		"error /increment: 'not' failed",
		"error /ownedby: ",
	)
	// a drop does not read the state file
	expectIssues(t, results, "v1_0_1.json",
		"error /deltas/1/name: sequence 'absent' has no valid state file (state/absent-sequence.json)",
		"error /deltas/3/name: sequence 'invoice_no' has no comment in state/invoice_no-sequence.json",
	)
	expectIssues(t, results, "v1_0_2.json",
		"error /deltas/0: additional properties 'value' not allowed",
		"error /deltas/0/action: value must be one of 'add', 'drop', 'alter'",
	)
	if kind, ok := KindOf("state/invoice_no-sequence.json"); !ok || kind != KindSequence {
		t.Errorf("EXPECTED a sequence file kind  ACTUAL: %s", kind)
	}
}
