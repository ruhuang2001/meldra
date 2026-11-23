import os
import sqlite3
import sys


mode, path, limit, *ids = sys.argv[1:]
db = sqlite3.connect(path, isolation_level=None)
db.execute('PRAGMA synchronous=OFF')
if mode == 'wal':
    db.execute('PRAGMA journal_mode=WAL')
    db.execute('PRAGMA wal_autocheckpoint=0')
    db.execute('CREATE TABLE wal_fixture(id INTEGER PRIMARY KEY, payload BLOB)')
    db.execute('INSERT INTO wal_fixture VALUES(1, zeroblob(1048576))')
    while os.stat(path + '-wal').st_size <= int(limit):
        db.execute('UPDATE wal_fixture SET payload=randomblob(1048576) WHERE id=1')
    os._exit(0)
elif mode == 'index':
    db.execute('PRAGMA journal_mode=DELETE')
    db.execute('DROP INDEX tools_recovery')
    db.execute('DROP INDEX approvals_call')
    db.execute('''WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
        INSERT INTO tool_calls(id,task_id,run_id,status,planned,record)
        SELECT printf('fixture-%08d',x),?,?,'succeeded','2026-10-08',
        json_object('id',printf('fixture-%08d',x),'task_id',?,'run_id',?,'name','read_file','arguments',json('{}'),'effect','read','status','succeeded','result',json('{"status":"succeeded"}')) FROM n''', ids * 2)
    db.execute('CREATE TABLE migration_padding(bytes BLOB)')
    while os.stat(path).st_size <= int(limit) - (2 << 20):
        db.execute('INSERT INTO migration_padding VALUES(zeroblob(1048576))')
    db.close()
else:
    raise ValueError(mode)
