-- Platform-level storefront announcement, mirroring partner_announcements without the partner FK.
CREATE TABLE platform_announcements(
 owner text PRIMARY KEY,
 enabled boolean NOT NULL DEFAULT false,
 placement text NOT NULL CHECK(placement IN ('home','product','both')),
 title text NOT NULL DEFAULT '',
 content text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 0 CHECK(version>=0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
