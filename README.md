# Tugas Besar IF3110 Pengembangan Aplikasi Web Milestone 1 - K03 G06

![](https://media1.tenor.com/m/SgzzUiQHxKkAAAAC/kasane-teto-teto.gif)

Platform web untuk mengelola akses ke database PostgreSQL yang digunakan bersama. Platform Admin membuat user dan group, mendaftarkan koneksi database, serta menentukan group mana yang mendapat tingkat akses apa pada database mana. User kemudian login untuk melihat schema dan data dari database yang dapat mereka akses, dan (jika memiliki akses edit) mengubahnya melalui migration yang berversi dan berjalan dalam transaction, dengan riwayat yang tersimpan untuk setiap database.

```
Login -> Access Control -> Select Database -> View Schema/Data
      -> Create Migration -> Execute -> Commit/Rollback -> Migration History
```

## Daftar Isi

- [Fitur](#fitur)
- [Teknologi](#teknologi)
- [Menjalankan Program](#menjalankan-program)
- [Keputusan Desain](#keputusan-desain)
  - [Autentikasi dan session](#autentikasi-dan-session)
  - [Otorisasi dan akses efektif](#otorisasi-dan-akses-efektif)
  - [Database viewer](#database-viewer)
  - [Migration](#migration)
  - [Analisis execution plan](#analisis-execution-plan)
- [Daftar Route](#daftar-route)
- [Struktur Proyek](#struktur-proyek)
- [Schema Database Platform](#schema-database-platform)
- [Pengujian](#pengujian)
- [Keterbatasan](#keterbatasan)

## Fitur

**Access control**
- Login dengan username dan password; password disimpan dengan hash bcrypt.
- Session di sisi server dengan identifier acak yang opaque di cookie `HttpOnly`, idle timeout, absolute expiration, rotasi berkala dengan grace period singkat, dan perlindungan CSRF pada setiap request yang mengubah state.
- Setiap aksi diotorisasi di backend, bukan hanya disembunyikan dari antarmuka.

**Administrasi platform** (khusus Platform Admin)
- User: membuat, mengubah nama, memberi atau mencabut status admin, mengaktifkan atau menonaktifkan, reset password, menghapus.
- Group: membuat, mengubah nama, menghapus, menambah atau mengeluarkan anggota.
- Koneksi database: membuat, mengubah, menghapus (kredensial disimpan terenkripsi).
- Pemberian akses: per group dan per database, yaitu **View Only** atau **View + Edit**.

**Pengelolaan database**
- Melihat schema, table, column, constraint, index, dan data table dengan pagination.
- Pesan yang jelas untuk host yang tidak terjangkau, kredensial salah, database tidak ditemukan, timeout, dan akses yang tidak diizinkan.

**Migration**
- Editor SQL migration dengan langkah draft, ubah, preview, dan eksekusi.
- Riwayat versi yang linear untuk setiap database, berisi versi, SQL, pembuat, pelaksana, waktu, durasi, dan status.
- Setiap migration berjalan dalam satu transaction: commit jika berhasil, rollback jika gagal.
- Log perubahan database yang mencatat migration dan statement dari SQL editor.

**Bonus**
- Analisis execution plan (`EXPLAIN` / `EXPLAIN ANALYZE`) dengan tampilan plan tree, cost, estimasi row, waktu, dan peringatan.

## Teknologi

| Bidang | Pilihan |
| --- | --- |
| Backend | Go 1.26, `net/http` dari standard library |
| Frontend | Halaman server-rendered dengan Go `html/template` (bukan SPA) |
| Driver PostgreSQL | [`pgx` v5](https://github.com/jackc/pgx) |
| Hashing password | bcrypt (`golang.org/x/crypto`) |
| Enkripsi kredensial | AES-256-GCM |
| Database | PostgreSQL 17 (database platform dan database target yang dikelola) |
| Packaging | Docker dan Docker Compose |

## Menjalankan Program

Prasyarat: Docker dengan plugin Compose.

1. **Buat file environment.**

   ```sh
   cp .env.example .env
   ```

2. **Isi nilai di `.env`.** Minimal, isi nilai yang unik untuk:

   | Variabel | Catatan |
   | --- | --- |
   | `PLATFORM_DB_PASSWORD` | Password database platform |
   | `TARGET_DB_PASSWORD` | Password server database target untuk demo |
   | `BOOTSTRAP_ADMIN_USERNAME` / `BOOTSTRAP_ADMIN_PASSWORD` | Platform Admin pertama. Password harus 12–72 byte |
   | `DB_CREDENTIAL_KEY` | Base64 dari 32 byte acak: `openssl rand -base64 32` |

3. **Memulai program**

   ```sh
   docker compose up --build
   ```

   Perintah ini menjalankan aplikasi, database platform (`platform-db`), dan server database target untuk demo (`target-db`). Migration untuk schema platform diterapkan otomatis saat startup, dan admin bootstrap dibuat jika belum ada.

4. **Buka <http://localhost:8080>** dan login dengan `BOOTSTRAP_ADMIN_USERNAME` / `BOOTSTRAP_ADMIN_PASSWORD`.

Untuk menghentikan, jalankan `docker compose down`. Untuk sekaligus menghapus seluruh data tersimpan (kedua database), jalankan `docker compose down -v`.

> Jika berjalan di balik HTTPS, set `COOKIE_SECURE=true` (atau `APP_ENV=production`) agar cookie session mendapat atribut `Secure`.

## Keputusan Desain

### Autentikasi dan session

| Pengaturan | Nilai |
| --- | --- |
| Idle timeout | 30 menit tanpa digunakan |
| Absolute lifetime | 12 jam sejak login; tidak pernah diperpanjang oleh aktivitas maupun rotasi |
| Interval rotasi | Identifier baru diterbitkan pada request pertama setelah 15 menit |
| Grace period | Identifier lama tetap berlaku selama 30 detik setelah rotasi (dan tidak pernah melewati absolute expiry) |
| Cookie | Nama tetap `cookie_session`; `HttpOnly`, `SameSite=Lax`, dan `Secure` bila diaktifkan |

Cara kerjanya:

- **Identifier opaque.** Session identifier berupa 256 bit acak. Hanya hash SHA-256-nya yang disimpan di database; nilai aslinya hanya ada di cookie. Identifier dan password tidak pernah muncul di URL, response, maupun log.
- **Rotasi.** Identifier diganti saat login, secara berkala (lihat di atas), dan setelah perubahan privilege. Nama cookie tetap sama, hanya nilainya yang berubah. Rotasi dilakukan dengan row lock, sehingga request paralel yang membawa cookie yang sama hanya memutarnya satu kali.
- **Grace period.** Identifier lama tetap dapat dipakai beberapa detik untuk menangani response yang tidak diterima dan request paralel. Selama jendela itu identifier lama diterima untuk request baca, tetapi request yang mengubah state dengan identifier lama ditolak.
- **Kedaluwarsa.** Session berakhir jika terlalu lama idle atau mencapai absolute lifetime. Setelah itu user harus login kembali.
- **Pencabutan.** Logout mencabut session di backend. Session user yang terdampak juga dicabut ketika admin menonaktifkan, mengubah, atau menghapus user, mereset password, mengubah keanggotaan group, menghapus group, mengubah atau mencabut grant, atau mengubah atau menghapus koneksi. Ketika admin mengubah akunnya sendiri, session diperbarui, bukan dicabut.
- **CSRF.** Setiap request yang mengubah state membawa token per-session yang diperiksa terhadap session. Form login memakai signed double-submit cookie karena belum ada session.
- **Password.** bcrypt, dengan panjang wajib 12–72 byte. Login yang gagal mengembalikan pesan generik yang sama, baik user tidak dikenal, nonaktif, maupun password salah.

### Otorisasi dan akses efektif

Ada dua konsep yang terpisah:

1. **Platform Admin** adalah peran tingkat platform. Peran ini mengelola user, group, koneksi, dan grant. Peran ini **bukan** tingkat akses pada database mana pun, sehingga admin yang tidak memiliki grant lewat group tidak dapat melihat atau menjalankan migration pada database.
2. **Akses database** diberikan kepada group, tidak pernah langsung kepada user:

   | Tingkat | Dapat melakukan |
   | --- | --- |
   | **View Only** | Melihat schema, table, data, execution plan, serta riwayat dan detail migration |
   | **View + Edit** | Semua yang ada pada View Only, ditambah membuat, mengubah, dan menjalankan migration serta memakai SQL editor |

**Akses efektif.** Seorang user dapat menjadi anggota beberapa group, dan satu group dapat memiliki tingkat berbeda pada setiap database. Jika beberapa group milik user memiliki grant pada database yang sama, **tingkat tertinggi yang berlaku** (View + Edit mengalahkan View Only). User tanpa grant pada suatu database tidak mendapat akses. Akses dihitung ulang pada setiap request, sehingga perubahan langsung berlaku.

Setiap method service memeriksa izinnya sendiri, sehingga perlindungan tidak bergantung pada handler mana yang memanggilnya. Admin tidak dapat menonaktifkan atau menghapus dirinya sendiri, maupun mencabut status admin miliknya sendiri.

### Database viewer

- Menampilkan daftar schema, lalu table pada schema yang dipilih beserta jumlah row.
- Untuk table yang dipilih, menampilkan column (tipe, nullability, default), constraint, index, dan data sebanyak 50 row per halaman.
- Koneksi ke target dibuka dengan connect timeout 5 detik dan statement timeout 10 detik. Penampilan data selalu memakai session read-only.
- Password koneksi hanya didekripsi saat koneksi dibuka dan tidak pernah ditampilkan maupun dicatat di log. Kegagalan koneksi dipetakan ke pesan yang aman untuk user (autentikasi gagal, database tidak ditemukan, akses ditolak, terlalu banyak koneksi, timeout, host tidak terjangkau).

### Migration

Migration adalah skrip SQL berversi yang disimpan per database. Siklus hidupnya:

```
DRAFT --ubah--> DRAFT --eksekusi--> SUCCESS   (final, tidak dapat diubah lagi)
                      --eksekusi--> FAILED    (di-rollback; dapat diubah dan dicoba lagi)
```

- **Riwayat linear.** Versi bernomor 1, 2, 3, … per database. Setiap database target menyimpan tabel ledger `public._dbmp_migrations`, dan sebuah migration hanya dapat diterapkan jika versinya adalah versi berikutnya setelah versi terakhir yang diterapkan. Versi sebelumnya harus berhasil terlebih dahulu.
- **Eksekusi transaksional.** Seluruh skrip berjalan dalam satu transaction di target, dan baris ledger ditulis dalam transaction yang sama. Error apa pun me-rollback semuanya, dan migration dicatat sebagai `FAILED` beserta pesan error dari database. Skrip yang berisi perintah transaksi sendiri (`BEGIN`, `COMMIT`, …) ditolak.
- **Tanpa eksekusi ganda.** Catatan percobaan di sisi platform hanya mengizinkan satu percobaan yang sedang berjalan per migration, dan advisory lock di target menserialisasi eksekusi yang bersamaan.
- **Jejak audit.** Riwayat migration menampilkan versi, judul, SQL, status, durasi, pembuat, pelaksana, dan waktu. Log perubahan juga mencatat statement ad-hoc dari SQL editor (sumber `SQL_EDITOR`), sehingga setiap perubahan pada database dapat ditelusuri.
- **Izin.** Hanya user View + Edit yang dapat membuat, mengubah, atau menjalankan migration dan memakai SQL editor. User View Only dapat melihat riwayat dan detail. Backend juga membatasi statement View Only hanya pada query baca dan membuka session mereka sebagai read-only.
- **Batas.** SQL maksimal 64 KiB, dan hasil query dipotong hingga 200 row.

### Analisis execution plan

Fitur bonus. Pada halaman database, user dapat menjalankan **Estimate** (`EXPLAIN`) atau **Analyze** (`EXPLAIN ANALYZE`) untuk satu query `SELECT` atau `WITH … SELECT`. Hasilnya ditampilkan sebagai plan tree berisi jenis node, nama relation dan index, jenis join, estimasi cost dan row, row dan waktu aktual (untuk Analyze), planning dan execution time, serta peringatan untuk sequential scan dan selisih besar antara estimasi dan jumlah row aktual. Fitur ini selalu berjalan di session read-only, untuk kedua tingkat akses.

## Daftar Route

| Route | Deskripsi |
| --- | --- |
| `GET /health` | Health check (memeriksa konektivitas ke database platform) |
| `GET /`, `GET /login`, `POST /login`, `POST /logout` | Halaman awal, login, logout |
| `GET /dashboard` | Dashboard setelah login |
| `GET /databases` | Database yang dapat diakses user saat ini |
| `GET /databases/{id}` | Viewer schema, table, dan data, detail migration, riwayat, dan log perubahan |
| `POST /databases/{id}/query` | SQL editor |
| `POST /databases/{id}/explain` | Analisis execution plan |
| `POST /databases/{id}/migrations` | Membuat draft migration |
| `POST /databases/{id}/migrations/preview` | Preview statement dari sebuah migration |
| `POST /databases/{id}/migrations/{version}/update` | Mengubah migration yang belum berhasil |
| `POST /databases/{id}/migrations/{version}/execute` | Menjalankan migration dalam transaction |
| `GET /admin` | Halaman administrasi platform (khusus Platform Admin) |
| `POST /admin/users`, `…/users/update`, `…/users/status`, `…/users/password`, `…/users/delete` | Pengelolaan user |
| `POST /admin/groups`, `…/groups/rename`, `…/groups/delete`, `POST /admin/memberships` | Pengelolaan group dan keanggotaan |
| `POST /admin/connections`, `…/connections/update`, `…/connections/delete` | Pengelolaan koneksi |
| `POST /admin/grants` | Memberi atau mencabut akses group ke sebuah database |

## Struktur Proyek

```
.
├── internal/
│   ├── main.go            # Titik masuk aplikasi dan wiring
│   ├── authentication/    # Login, session, aturan rotasi (fungsi murni Decide)
│   ├── authorization/     # Tingkat akses, akses efektif, pemeriksaan platform admin
│   ├── admin/             # Service admin: user, group, koneksi, grant
│   ├── explorer/          # Viewer, SQL editor, migration, analisis plan (lapisan service)
│   ├── database/          # Implementasi PostgreSQL: store platform, akses ke target
│   ├── domain/            # Tipe bersama (User, Group, Connection)
│   ├── http/              # Handler, router, security header
│   └── migration/         # SQL migration untuk database platform (di-embed)
├── frontend/              # Template Go (pages/) dan aset statis (assets/)
├── seed/                  # Program data demo
├── scripts/test.sh        # Test runner (unit dan integration)
├── compose.yaml           # App, platform-db, target-db
└── dockerfile             # Build multi-stage untuk binary server dan seed
```

Pelapisannya disengaja: handler hanya mengurus HTTP, service (`admin`, `explorer`, `authentication`) menampung aturan bisnis dan otorisasi, dan `database` hanya menyimpan dan mengambil data. Access control berada di `authorization` dan service, terpisah dari autentikasi.

## Schema Database Platform

Diterapkan otomatis saat startup oleh migration runner yang di-embed (dicatat di `schema_migrations`).

| Table | Fungsi |
| --- | --- |
| `users` | Akun: username, hash password, flag admin, flag aktif |
| `groups`, `group_members` | Group beserta anggotanya |
| `database_connections` | Database yang dikelola beserta password koneksi terenkripsi |
| `group_database_access` | Tingkat akses (`VIEW_ONLY` / `VIEW_EDIT`) per group dan database |
| `sessions` | Hash identifier, identifier sebelumnya beserta batas grace-nya, token CSRF, timestamp, pencabutan |
| `migrations` | Migration berversi per database beserta status, error, dan durasi |
| `migration_attempts` | Satu baris per percobaan eksekusi; mencegah eksekusi bersamaan |
| `database_change_log` | Log audit migration dan statement dari SQL editor |

## Pengujian

```sh
./scripts/test.sh          # unit test saja
./scripts/test.sh -i       # unit + integration test (menjalankan PostgreSQL sekali pakai di Docker)
./scripts/test.sh -i -run Target   # argumen tambahan diteruskan ke `go test`
```

Anda juga dapat menjalankan `go test ./...` secara langsung. Integration test dilewati kecuali variabel berikut mengarah ke server PostgreSQL yang boleh dipakai bebas:

| Variabel | Dipakai oleh |
| --- | --- |
| `TEST_DATABASE_URL` | Integration test database platform (setiap test bekerja di schema-nya sendiri) |
| `TEST_POSTGRES_DSN` | Integration test database target dan explorer end-to-end |

Cakupan pengujian:

- **Unit test:** aturan session (idle, absolute, rotasi, grace), hashing password, tingkat akses dan akses efektif, service admin (izin dan validasi input), pemisahan statement SQL dan deteksi read-only, penanganan execution plan, dan enkripsi kredensial.
- **HTTP test:** login dan logout dengan CSRF, atribut cookie, route admin (autentikasi, khusus admin, CSRF, identifier lama), dan route database untuk setiap tingkat akses.
- **Integration test PostgreSQL:** siklus hidup session dan rotasi bersamaan, penolakan identifier lama untuk request yang mengubah state, otorisasi admin lewat HTTP, siklus hidup percobaan migration, migration berversi dan transaksional, serta satu alur explorer end-to-end.

## Keterbatasan

- Hanya PostgreSQL yang didukung sebagai database yang dikelola. Dukungan DBMS lain belum diimplementasikan.
- Platform membutuhkan akun database di setiap target yang diizinkan membuat tabel ledger `public._dbmp_migrations` dan menjalankan migration.
- Analisis execution plan hanya menerima query baca. Fitur ini belum menampilkan plan untuk migration DDL.
- Belum ada pembatasan laju login (rate limiting) maupun penguncian akun.
- Password bawaan seed sengaja lemah; set `SEED_USER_PASSWORD` untuk penggunaan di luar demo lokal.