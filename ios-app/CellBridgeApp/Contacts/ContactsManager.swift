import Foundation
import Contacts

/// Reads the system address book and provides name resolution for phone numbers.
final class ContactsManager: ObservableObject {
    @Published var contacts: [ContactEntry] = []
    @Published var authorized = false
    
    private let store = CNContactStore()
    private var cache: [String: String] = [:] // normalized number -> display name
    
    struct ContactEntry: Identifiable, Hashable {
        let id: String // CNContact.identifier
        let name: String
        let phones: [String] // formatted numbers
        let thumbnailData: Data?
    }
    
    func requestAccess() {
        if #available(iOS 18.0, *) {
            Task {
                do {
                    let granted = try await store.requestAccess(for: .contacts)
                    await MainActor.run { self.authorized = granted }
                    if granted { self.loadContacts() }
                } catch {
                    NSLog("CellBridge: contacts access error: \(error)")
                }
            }
        } else {
            store.requestAccess(for: .contacts) { [weak self] granted, error in
                DispatchQueue.main.async { self?.authorized = granted }
                if granted { self?.loadContacts() }
                if let error { NSLog("CellBridge: contacts access error: \(error)") }
            }
        }
    }
    
    func loadContacts() {
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            let keys: [CNKeyDescriptor] = [
                CNContactGivenNameKey as CNKeyDescriptor,
                CNContactFamilyNameKey as CNKeyDescriptor,
                CNContactPhoneNumbersKey as CNKeyDescriptor,
                CNContactThumbnailImageDataKey as CNKeyDescriptor,
                CNContactOrganizationNameKey as CNKeyDescriptor,
            ]
            let request = CNContactFetchRequest(keysToFetch: keys)
            request.sortOrder = .familyName
            
            var entries: [ContactEntry] = []
            var nameCache: [String: String] = [:]
            
            do {
                try self.store.enumerateContacts(with: request) { contact, _ in
                    let fullName: String
                    let given = contact.givenName.trimmingCharacters(in: .whitespaces)
                    let family = contact.familyName.trimmingCharacters(in: .whitespaces)
                    if !given.isEmpty || !family.isEmpty {
                        fullName = "\(family)\(given)"
                    } else if !contact.organizationName.isEmpty {
                        fullName = contact.organizationName
                    } else {
                        return // skip contacts with no name
                    }
                    
                    let phones = contact.phoneNumbers.map { $0.value.stringValue }
                    guard !phones.isEmpty else { return }
                    
                    entries.append(ContactEntry(
                        id: contact.identifier,
                        name: fullName,
                        phones: phones,
                        thumbnailData: contact.thumbnailImageData
                    ))
                    
                    for phone in phones {
                        let normalized = Self.normalizeNumber(phone)
                        nameCache[normalized] = fullName
                    }
                }
            } catch {
                NSLog("CellBridge: contacts fetch error: \(error)")
            }
            
            DispatchQueue.main.async {
                self.contacts = entries
                self.cache = nameCache
            }
        }
    }
    
    /// Resolves a phone number to a contact name (or returns the number as-is).
    func displayName(for number: String) -> String {
        let normalized = Self.normalizeNumber(number)
        return cache[normalized] ?? number
    }
    
    /// Returns true if this number belongs to a known contact.
    func isKnownContact(_ number: String) -> Bool {
        cache[Self.normalizeNumber(number)] != nil
    }
    
    /// Strip everything except digits and leading +.
    static func normalizeNumber(_ raw: String) -> String {
        let cleaned = raw.filter { $0.isNumber || $0 == "+" }
        // Handle Chinese numbers: if starts with +86, keep; if 11 digits starting with 1, add +86
        if cleaned.hasPrefix("+") { return cleaned }
        if cleaned.count == 11, cleaned.hasPrefix("1") { return "+86" + cleaned }
        return cleaned
    }
}
