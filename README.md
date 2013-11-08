gmailsync
=========

Maintain an offline copy of a GMail account.

Why?
====

 - It's easy to deploy. Download a single binary, add your account
   details to a config file and start syncing. Compatible with Mac OS X,
   Linux and Windows.

 - It's efficient. All mails are stored compressed in a single archive
   file, typically requiring about half the space of what GMail reports
   as your usage..

 - It's safe. All messages are cryptographically hashed to ensure their
   integrity and the archive format is simple, open and documented.
   Messages, once written, are never altered or removed.

 - It's portable. The archive can be exported to a standard format MBOX
   file, readable by most email programs and easily convertable to other
   storage formats.


Archive File Format
===================

The archive file is a simple append-only sequence of records, preceded
by a header record.

Archive Header
--------------

     0                   1                   2                   3
     0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                          Magic Number                         |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |     Version   |                   Reserved                    |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                          Create Time                          |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                          Update Time                          |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                    Have Pointer (lower 32)                    |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                    Have Pointer (upper 32)                    |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                            Reserved                           |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                            Reserved                           |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                            Reserved                           |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                            Reserved                           |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

 - Magic Number (uint32): Always set to 0x20121025.

 - Version (uint8): Always set to 1.

 - Create Time (uint32): Time of archive creation, in seconds since the
   Unix epoch.

 - Update Time (uint32): Time of last successfull update, in seconds
   since the Unix epoch.

 - Have Pointer (uint64(: Offset in the archive, in bytes, where the
   most current Have Record may be found. Set to zero if there is no
   Have Record.

Record Structure
----------------

A record encapsulates some unspecified data (the "payload"). A record
has a type and the payload is optionally hashed and compressed.

     0                   1                   2                   3
     0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |             Type              |   Reserved Feature Bits   |H|C|
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                            Length                             |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
    |                             data                              |
    +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

 - Type (uint16): Record type number. One of the valid type numbers as
   defined below.

 - Feature Bits:

   - "C" (Compressed): Indicates that the payload data is compressed
     with gzip.

   - "H" (Hashed): Indicates that the data is hashed. The first 20 bytes
     of the data field is the SHA-1 hash of the payload. The hash is of
     the uncompressed data, irrespective of the Compressed bit. The data
     (compressed or cleartext) follows directly after the hash bytes.

 - Length (uint32): Length of data portion following the header fields,
   after compression (if "C" is set) and including hash (if "H" is set).

### Message Record (Type=1)

A Message Record represents a single email message. The Type field is
set to 1.

The data is an ASN.1 DER encoded structure of with the following layout:

    SEQUENCE MessageRecord
        INTEGER      MessageID
        OCTET STRING MessageData

 - MessageID: Message ID as used by Gmail to uniquely identify messages.
 - MessageData: Complete email message in RFC822 format as seen on the
   wire, including headers.

Message Records are compressed and hashed.

### Labels Record (Type=2)

A Labels Record represents a set of labels attached to email messages.

The data is an ASN.1 DER encoded structure of with the following layout:

    SEQUENCE LabelsRecord
        SEQUENCE LabelEntry
            INTEGER MessageID
            SEQUENCE
                OCTET STRING Label
                OCTET STRING ...
        SEQUENCE ...

 - MessageID: Message ID as used by Gmail to uniquely identify messages.
 - Label: A single string representing a label applied to a message.
 - The LabelsRecord contains one or more LabelEntry sequences.

Labels Records are compressed.

### Delete Record (Type=3)

The Delete Record is a list of Message IDs that are no longer present on
the server. When exporting the archive, messages with these Message IDs
should not be output by default.

The data is an ASN.1 DER encoded structure of with the following layout:

    SEQUENCE DeleteRecord
        INTEGER MessageID
        INTEGER ...

### Have Record (Type=4)

The Have Record is a list of Message IDs of all messages that exist in
the archive, less those marked as deleted, up to this point in the
archive.  It is used as a short cut to avoid having to read every
preceding record to build the current state when opening the archive.
The Have Pointer field in the archive header points to the most current
Have Record in the archive. To create an up to date list of messages in
the archive, read the archive header, skip to the offset denoted by the
Have Pointer, read the Have Record, then process any following records
as usual.

The data is an ASN.1 DER encoded structure of with the following layout:

    SEQUENCE HaveRecord
        INTEGER MessageID
        INTEGER ...

Interpretation
--------------

The archive is a sequence of Message and Labels Records in an
unspecified order. For any given message (identified by a unique Message
ID) there is exactly one Message Record containing the message data and
zero or more Labels Records. If a given Message ID is not mentioned in
any Labels Record in the archive, this is taken to mean there are no
labels applied to the message. If a given Message ID is mentioned in
more than one Labels Record, the later Labels Record supersedes prior
Labels Records for that message (i.e. there are no "diffs"; each Labels
Record represents the complete truth at that point in time).

