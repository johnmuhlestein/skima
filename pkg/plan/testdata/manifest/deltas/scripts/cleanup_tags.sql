delete from tags t where not exists (select 1 from documents d where d.id = t.document_id);
