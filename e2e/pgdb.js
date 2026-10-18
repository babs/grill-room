import { fileURLToPath } from 'node:url';

const pgdb = fileURLToPath(new URL('../bin/pgdb', import.meta.url));

// The shell command that starts a server on database db, dropped and made anew by bin/pgdb (make
// e2e-build) on the PostgreSQL server GRILL_ROOM_TEST_DATABASE_URL names.
export function withFreshDB(db, command) {
  if (!process.env.GRILL_ROOM_TEST_DATABASE_URL) {
    throw new Error('GRILL_ROOM_TEST_DATABASE_URL is unset: point it at a PostgreSQL 16 server (make pg-up prints one)');
  }
  // A bare assignment fails with its command substitution; export would hide that.
  return `GRILL_ROOM_DATABASE_URL="$('${pgdb}' fresh "$GRILL_ROOM_TEST_DATABASE_URL" ${db})" && export GRILL_ROOM_DATABASE_URL && ${command}`;
}
