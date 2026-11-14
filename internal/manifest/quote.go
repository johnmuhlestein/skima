package manifest

import (
	"regexp"
	"strings"
)

// plainIdentifier is a name postgres accepts without quotes. Upper case letters are allowed: unquoted, postgres folds
// them to lower case, which is how skima has always created such names - quoting them now would change which object
// a name refers to
var plainIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// keywords are the postgres keywords that are not unreserved - the ones quote_ident quotes. From postgres 17:
// select word from pg_get_keywords() where catcode <> 'U'
var keywords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`all analyse analyze and any array as asc asymmetric authorization between bigint
		binary bit boolean both case cast char character check coalesce collate collation column concurrently constraint
		create cross current_catalog current_date current_role current_schema current_time current_timestamp current_user
		dec decimal default deferrable desc distinct do else end except exists extract false fetch float for foreign
		freeze from full grant greatest group grouping having ilike in initially inner inout int integer intersect
		interval into is isnull join json json_array json_arrayagg json_exists json_object json_objectagg json_query
		json_scalar json_serialize json_table json_value lateral leading least left like limit localtime localtimestamp
		merge_action national natural nchar none normalize not notnull null nullif numeric offset on only or order out
		outer overlaps overlay placing position precision primary real references returning right row select
		session_user setof similar smallint some substring symmetric system_user table tablesample then time timestamp
		to trailing treat trim true union unique user using values varchar variadic verbose when where window with
		xmlattributes xmlconcat xmlelement xmlexists xmlforest xmlnamespaces xmlparse xmlpi xmlroot xmlserialize
		xmltable`) {
		keywords[word] = true
	}
}

// QuoteIdent returns a name ready to use in generated DDL: as it is when postgres accepts it unquoted, otherwise in
// double quotes - a keyword such as user or order, or a name with spaces or other characters. Names that worked
// before keep producing the same SQL
func QuoteIdent(name string) string {
	if plainIdentifier.MatchString(name) && !keywords[strings.ToLower(name)] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteList joins a list of names with sep - a column list. An entry is quoted only when it is a plain name that needs
// it (a keyword such as user or order): anything else is used as written, as before - several columns in one entry
// ("a, b"), or in an index an expression or a sort order (lower(email), label desc)
func quoteList(names []string, sep string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = name
		if plainIdentifier.MatchString(name) {
			quoted[i] = QuoteIdent(name)
		}
	}
	return strings.Join(quoted, sep)
}

// quoteQualified quotes each dot-separated part of a qualified name, e.g. table.column
func quoteQualified(name string) string {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		parts[i] = QuoteIdent(part)
	}
	return strings.Join(parts, ".")
}
