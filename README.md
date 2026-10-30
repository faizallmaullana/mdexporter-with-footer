# Markdown to PDF exporter

CLI Go kecil untuk mencetak Markdown ke PDF ukuran A4 dengan `public/footer.png`
di setiap halaman. Konversi Markdown dilakukan langsung oleh program; Chromium/Chrome
digunakan untuk mencetak PDF.

## Prasyarat

- Go
- Chromium atau Google Chrome yang tersedia di `PATH`

## Build dan instalasi

```sh
make build
sudo make install
```

Binary dipasang sebagai `/usr/bin/mdexport`, sedangkan footer utama disimpan di
`/usr/src/footer`. Lokasi lama `/usr/src/pdfexporter/footer.png` tetap dipakai
sebagai fallback. `make install` memerlukan hak akses root.

## Penggunaan

```sh
go run . not-261030001_kickoff_rapat.md
```

PDF dibuat di samping file Markdown dengan nama yang sama. Lokasi output dan
footer dapat diubah. Margin konten dapat diatur terpisah dalam milimeter; default
atas 5 mm, kanan 15 mm, bawah 25 mm, kiri 15 mm:

Margin cetak halaman mengikuti lampiran: atas 5 mm dan bawah 25 mm. Margin
halaman kiri/kanan tetap 0 agar footer bisa memenuhi lebar kertas; konten memakai
padding kiri/kanan masing-masing 15 mm.

```sh
go run . -out hasil.pdf -margin-top 5 -margin-right 15 \
  -margin-bottom 25 -margin-left 15 catatan.md
```

Ganti footer terpasang dengan file lain (perlu hak akses untuk menulis ke
`/usr/src`):

```sh
sudo go run . -changefooter filename.png
```

Atau setelah instalasi:

```sh
sudo mdexport -changefooter filename.png
```
