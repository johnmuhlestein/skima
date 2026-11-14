create or replace function update_last_modified_at() returns trigger language plpgsql as $$
begin
  new.last_modified_at = now();
  return new;
end;
$$;
