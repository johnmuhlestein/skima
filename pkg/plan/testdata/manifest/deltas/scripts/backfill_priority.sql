update documents set priority = 1 where document_type = 'STRUCTURED';
update documents set priority = 0 where priority is null;
